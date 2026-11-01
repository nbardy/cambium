#!/usr/bin/env python3
"""Measure selectable unused-cache retention with real offline npm workflows.

The before binary uses Cambium's normal 168-hour retention policy. The after
binary uses an explicit zero unused-cache budget. The existing age-based
`--older-than 1ns` command can also reclaim all unreferenced entries; this is a
retention-policy comparison, not evidence of faster copying or unique cleanup.
Each workflow retains the Git-plus-copied-environment control. A separate
multi-generation scenario verifies selective retention and checkpoint replay.

Commands run only in newly created disposable repositories. Successful scratch
data is removed; failures retain their scratch tree, JSON, and command logs.
"""

from __future__ import annotations

import argparse
import contextlib
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import shutil
import stat
import sys
import tempfile
import time
from typing import Any


HERE = Path(__file__).resolve().parent
SPEC = importlib.util.spec_from_file_location("workflow_benchmark", HERE / "workflow-benchmark.py")
workflow = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(workflow)
MIB = 1024 * 1024


def regular_bytes(root: Path) -> int:
    """Match the budget's logical regular-file accounting without following links."""
    total = 0
    for directory, _, names in os.walk(root, followlinks=False):
        for name in names:
            entry = (Path(directory) / name).lstat()
            if stat.S_ISREG(entry.st_mode):
                total += entry.st_size
    return total


def inventory(reference: Path) -> list[dict[str, Any]]:
    result = []
    state = reference / ".git" / "cambium"
    for kind, directory in (("baseline", "baselines"), ("layer", "layers")):
        root = state / directory
        if not root.exists():
            continue
        for entry in sorted(root.iterdir()):
            if entry.is_symlink() or not entry.is_dir():
                raise ValueError(f"unexpected cache inventory entry: {entry}")
            ready = entry / "ready"
            result.append({"kind": kind, "id": entry.name, "logical_bytes": regular_bytes(entry), "ready_mtime_ns": (ready if ready.exists() else entry).stat().st_mtime_ns})
    return result


def make_source(root: Path, recorder: workflow.Recorder, payload_bytes: int) -> tuple[Path, dict[str, Any]]:
    """Use the reviewed npm fixture and add an explicit, deterministic warm cache."""
    source = root / "source"
    shutil.copytree(HERE / "workflow-fixtures" / "npm", source)
    recipe = json.loads((source / "recipe.json").read_text())
    (source / "recipe.json").unlink()
    manifest = json.loads((source / "package.json").read_text())
    manifest.update({"cache_bytes": payload_bytes, "cache_byte": 65})
    (source / "package.json").write_text(json.dumps(manifest, indent=2) + "\n")
    (source / "seed-cache.mjs").write_text("""import { mkdir, readFile, writeFile } from 'node:fs/promises';
const manifest = JSON.parse(await readFile('package.json', 'utf8'));
await mkdir('node_modules/.cache', { recursive: true });
await writeFile('node_modules/.cache/workflow-seed.bin', Buffer.alloc(manifest.cache_bytes, manifest.cache_byte));
""")
    (source / "verify-cache.mjs").write_text("""import { readFile } from 'node:fs/promises';
import { createHash } from 'node:crypto';
import assert from 'node:assert/strict';
const manifest = JSON.parse(await readFile('package.json', 'utf8'));
const actual = await readFile('node_modules/.cache/workflow-seed.bin');
const digest = value => createHash('sha256').update(value).digest('hex');
assert.equal(actual.length, manifest.cache_bytes);
assert.equal(digest(actual), digest(Buffer.alloc(manifest.cache_bytes, manifest.cache_byte)));
""")
    for name in ("build.mjs", "validate.mjs"):
        path = source / name
        path.write_text(path.read_text() + "\nimport './verify-cache.mjs';\n")
    policy = source / ".cambium.toml"
    policy_text = policy.read_text()
    inputs_line = 'inputs = ["package.json", "package-lock.json", "packages/**", "validate.mjs"]'
    if policy_text.count(inputs_line) != 1:
        raise ValueError("npm fixture must contain exactly the expected input-pattern declaration")
    policy.write_text(policy_text.replace(inputs_line, inputs_line[:-1] + ', "seed-cache.mjs", "verify-cache.mjs"]'))
    recipe["prepare"].append(["node", "seed-cache.mjs"])
    recipe["dependency_prepare"].append(["node", "seed-cache.mjs"])
    recipe.update({"name": "offline-npm-cache-budget", "source": str(source), "ref": "HEAD"})
    recipe["provenance"] = {
        "type": "offline-npm-storage-fixture",
        "description": "Real local npm dependency installation, build and Node tests, with a deterministic warm node_modules/.cache payload checked by the build and trusted validator. Synthetic retention workload, not a maintained-project or monorepo performance claim.",
        "warm_cache_payload_bytes": payload_bytes,
    }
    for command in (
        ["git", "init", "-q", "-b", "main"],
        ["git", "config", "user.name", "Cambium Cache Budget Benchmark"],
        ["git", "config", "user.email", "benchmark@example.invalid"],
        ["npm", "install", "--package-lock-only", "--offline", "--ignore-scripts", "--no-audit", "--no-fund"],
        ["git", "add", "."],
        ["git", "commit", "-q", "-m", "Reproducible npm cache retention fixture"],
    ):
        recorder.run(command, source, "fixture")
    return source, workflow.validate_recipe(recipe)


def retention_scenario(root: Path, source: Path, recipe: dict[str, Any], binary: str, recorder: workflow.Recorder) -> dict[str, Any]:
    method_root = root / "retention"
    method_root.mkdir()
    reference = method_root / "reference"
    result: dict[str, Any] = {"status": "running", "phases": [], "generations": []}
    recorder.report["retention"] = result
    recorder.run(["git", "clone", "--quiet", "--no-local", str(source), str(reference)], method_root, "retention_setup")
    for key, value in (("user.name", "Cambium Cache Budget Benchmark"), ("user.email", "benchmark@example.invalid")):
        recorder.run(["git", "config", key, value], reference, "retention_setup")
    recorder.run([binary, "init", "--allow-policy-commands"], reference, "retention_setup")
    start_free = workflow.disk_point(method_root)["volume_free_bytes"]

    @contextlib.contextmanager
    def phase(name: str):
        print(f"cache-budget: {name}", flush=True)
        stage: dict[str, Any] = {"name": name, "started_at": workflow.utc_now(), "status": "running", "before": workflow.disk_point(method_root, start_free)}
        result["phases"].append(stage)
        recorder.checkpoint()
        began = time.monotonic()
        try:
            yield stage
            stage["status"] = "ok"
        except BaseException as error:
            stage.update({"status": "failed", "error": str(error)})
            raise
        finally:
            stage["elapsed_seconds"] = time.monotonic() - began
            stage["after"] = workflow.disk_point(method_root, start_free)
            stage["volume_reclaimed_bytes"] = stage["after"]["volume_free_bytes"] - stage["before"]["volume_free_bytes"]
            recorder.checkpoint()

    def command(argv: list[str], phase_name: str) -> str:
        return recorder.run(argv, reference, phase_name, "retention")

    def prune(budget: int, phase_name: str, dry_run: bool = False) -> dict[str, Any]:
        args = [binary, "prune", "--max-unused-bytes", str(budget), "--json"]
        if dry_run:
            args.append("--dry-run")
        return json.loads(command(args, phase_name))

    pinned = None
    for index, amount in enumerate((8 * MIB, 12 * MIB, 16 * MIB), 1):
        with phase(f"generation_{index}") as stage:
            manifest = json.loads((reference / "package.json").read_text())
            manifest.update({"cache_bytes": amount, "cache_byte": 64 + index})
            (reference / "package.json").write_text(json.dumps(manifest, indent=2) + "\n")
            command(["git", "add", "package.json"], stage["name"])
            command(["git", "commit", "-q", "--allow-empty", "-m", f"Cache generation {index}"], stage["name"])
            workflow.run_commands(recorder, recipe["prepare"], reference, stage["name"], "retention", index)
            workflow.run_commands(recorder, recipe["verify"], reference, stage["name"], "retention", index)
            name = f"cache-generation-{index}"
            workspace = method_root / name
            created = json.loads(command([binary, "create", "--materializer", "git", "--path", str(workspace), "--json", name], stage["name"]))
            workflow.run_commands(recorder, recipe["verify"], workspace, stage["name"], "retention", index)
            generation = {"index": index, "payload_bytes": amount, "workspace": created}
            if index == 1:
                pinned = json.loads(command([binary, "checkpoint", "--as", "cache/pinned", "--json", name], stage["name"]))
                generation["checkpoint"] = pinned
            command([binary, "remove", "--force", "--delete-branch", name], stage["name"])
            assert not workspace.exists(), "generation workspace survived removal"
            result["generations"].append(generation)
            stage["inventory"] = inventory(reference)

    assert pinned and pinned["environment_ids"], "fixture did not retain a checkpoint environment"
    protected_ids = set(pinned["environment_ids"])
    initial = inventory(reference)
    unused = [entry for entry in initial if entry["kind"] != "layer" or entry["id"] not in protected_ids]
    assert len(unused) >= 2, "fixture needs multiple unused cache generations"
    newest_ids = set(result["generations"][-1]["workspace"]["layer_ids"]) - protected_ids
    newest = [entry for entry in unused if entry["kind"] == "layer" and entry["id"] in newest_ids]
    budget = sum(entry["logical_bytes"] for entry in newest)
    assert budget >= 16 * MIB, "newest generation lacks the intended cache payload"
    result.update({"initial_inventory": initial, "protected_layer_ids": sorted(protected_ids), "selective_budget_bytes": budget, "newest_unused": newest})

    with phase("selective_dry_run") as stage:
        dry = prune(budget, stage["name"], dry_run=True)
        stage["prune_result"] = dry
        assert inventory(reference) == initial, "dry-run changed cache contents or ready times"
        assert dry["selected_bytes"] > 0 and dry["removed_bytes"] == 0
        assert dry["retained_bytes"] <= budget
        assert not any(entry["id"] in protected_ids for entry in dry["selected"])
        assert not any(entry["id"] in newest_ids for entry in dry["selected"])
        assert dry["protected_bytes"] >= 8 * MIB

    with phase("selective_prune") as stage:
        selected = prune(budget, stage["name"])
        stage["prune_result"] = selected
        assert {(entry["kind"], entry["id"]) for entry in selected["removed"]} == {(entry["kind"], entry["id"]) for entry in dry["selected"]}
        assert selected["removed_bytes"] == dry["selected_bytes"]
        remaining = inventory(reference)
        assert newest_ids.issubset({entry["id"] for entry in remaining})
        assert protected_ids.issubset({entry["id"] for entry in remaining})
        assert sum(entry["logical_bytes"] for entry in remaining if entry["kind"] != "layer" or entry["id"] not in protected_ids) <= budget
        stage["inventory"] = remaining

    with phase("zero_budget_with_checkpoint") as stage:
        zero = prune(0, stage["name"])
        stage["prune_result"] = zero
        assert zero["retained_bytes"] == 0 and zero["protected_bytes"] >= 8 * MIB
        remaining = inventory(reference)
        assert protected_ids == {entry["id"] for entry in remaining}
        stage["inventory"] = remaining

    with phase("replay_after_source_environment_deletion") as stage:
        # All original workspaces are already removed. Destroy the primary
        # environment, whose tracked inputs now describe generation three.
        shutil.rmtree(workflow.owned_path(reference, "node_modules"))
        assert not (reference / "node_modules").exists()
        replay = method_root / "replay"
        forked = json.loads(command([binary, "fork", "--materializer", "git", "--path", str(replay), "--json", "cache/pinned", "replay"], stage["name"]))
        stage["fork_result"] = forked
        workflow.run_commands(recorder, recipe["verify"], replay, stage["name"], "retention")
        restored = json.loads((replay / "package.json").read_text())
        assert restored["cache_bytes"] == 8 * MIB and restored["cache_byte"] == 65
        stage["verified_replayed_generation"] = 1
        command([binary, "remove", "--force", "--delete-branch", "replay"], stage["name"])
        assert not replay.exists()

    with phase("release_checkpoint_and_prune") as stage:
        command([binary, "root", "drop", "cache/pinned"], stage["name"])
        stage["root_gc"] = json.loads(command([binary, "root", "gc", "--older-than", "1ns", "--json"], stage["name"]))
        stage["prune_result"] = prune(0, stage["name"])
        assert inventory(reference) == [], "unreferenced cache remains after final zero budget"
        stage["inventory"] = []
    result["status"] = "ok"
    return result


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--before-bin", required=True)
    parser.add_argument("--after-bin", required=True)
    parser.add_argument("--before-source-commit", required=True)
    parser.add_argument("--after-source-commit", required=True)
    parser.add_argument("--output", required=True, type=Path)
    parser.add_argument("--scratch", type=Path)
    parser.add_argument("--count", type=int, default=2)
    parser.add_argument("--payload-bytes", type=int, default=16 * MIB)
    parser.add_argument("--keep", action="store_true")
    args = parser.parse_args(argv)
    if args.count < 1 or args.payload_bytes < 1:
        parser.error("--count and --payload-bytes must be positive")
    binaries = {}
    for name in ("before", "after"):
        binary = shutil.which(getattr(args, name + "_bin"))
        if not binary:
            parser.error(f"{name} binary is not executable")
        source_commit = getattr(args, name + "_source_commit")
        if len(source_commit) != 40 or any(char not in "0123456789abcdef" for char in source_commit):
            parser.error(f"{name} source commit must be a full lowercase Git commit ID")
        binaries[name] = {"path": str(Path(binary).resolve()), "sha256": hashlib.sha256(Path(binary).read_bytes()).hexdigest(), "source_commit": source_commit}
    output = args.output.resolve()
    reports = {name: output.with_name(output.stem + "-" + name + ".json") for name in ("before", "after")}
    if any(path.exists() for path in (output, *reports.values())):
        parser.error("an output report already exists; choose a new output path")
    output.parent.mkdir(parents=True, exist_ok=True)
    if args.scratch:
        args.scratch.mkdir(parents=True, exist_ok=True)
    root = Path(tempfile.mkdtemp(prefix="cambium-cache-budget-", dir=args.scratch)).resolve()
    report: dict[str, Any] = {
        "version": 1, "status": "running", "started_at": workflow.utc_now(), "scratch": str(root), "binaries": binaries,
        "harness_sha256": hashlib.sha256(Path(__file__).read_bytes()).hexdigest(),
        "measurement_notes": [
            "Before/default168h and after/zero-budget are different explicit retention policies, not an equal-policy speed comparison. The existing older-than1ns command can also delete all unreferenced cache entries.",
            "Each before/after workflow includes its own Git-plus-environment-copy control, using the same exact source commit. Runs are sequential, before then after; no statistical latency improvement is claimed.",
            "The npm fixture executes real installs, builds, tests, source edits and dependency edits, and checks a deterministic warm cache payload. This is a synthetic storage/correctness fixture, not a maintained-project performance result.",
            "Physical allocation is signed synchronized whole-volume free-space change. Unrelated writes and report/log growth can affect it; logical regular-file budget bytes are not a physical-allocation cap.",
            "A protected checkpoint's cache remains outside the unused budget. The separate retention scenario checks dry-run, selective eviction, zero budget and successful replay after original workspaces and primary environment are removed.",
        ],
    }
    recorder = workflow.Recorder(output, report, timeout=1800)
    success = False
    try:
        report["host"] = workflow.host_info(root, recorder, binaries["after"]["path"])
        source, recipe = make_source(root, recorder, args.payload_bytes)
        report["recipe"] = recipe
        report["source_commit"] = recorder.run(["git", "rev-parse", "HEAD"], source, "source").strip()
        report["source_tree"] = recorder.run(["git", "rev-parse", "HEAD^{tree}"], source, "source").strip()
        recipe_path = root / "recipe.json"
        workflow.write_json(recipe_path, recipe)
        report["workflows"] = {}
        for name, policy in (("before", ["--prune-older-than", "168h"]), ("after", ["--prune-max-unused-bytes", "0"])):
            recorder.run([sys.executable, str(HERE / "workflow-benchmark.py"), "--recipe", str(recipe_path), "--cambium-bin", binaries[name]["path"], "--methods", "git-env-copy,cambium-auto", "--count", str(args.count), "--scratch", str(root), "--output", str(reports[name]), *policy], root, name + "_workflow")
            result = json.loads(reports[name].read_text())
            assert result["status"] == "ok"
            assert result["resolved_source_commit"] == report["source_commit"]
            assert result["resolved_source_tree"] == report["source_tree"]
            report["workflows"][name] = {"report": reports[name].name, "sha256": hashlib.sha256(reports[name].read_bytes()).hexdigest(), "prune_policy": result["prune_policy"]}
            recorder.checkpoint()
        retention_scenario(root, source, recipe, binaries["after"]["path"], recorder)
        report["status"] = "ok"
        success = True
    except (Exception, KeyboardInterrupt) as error:
        report.update({"status": "failed", "error": str(error) or type(error).__name__})
        print(f"Cache budget benchmark failed: {report['error']}", file=sys.stderr)
    finally:
        report["completed_at"] = workflow.utc_now()
        report["scratch_retained"] = args.keep or not success
        recorder.checkpoint()
        if success and not args.keep:
            shutil.rmtree(root)
    print(f"Report: {output}")
    if report["scratch_retained"]:
        print(f"Scratch retained: {root}")
    return 0 if success else 1


if __name__ == "__main__":
    raise SystemExit(main())
