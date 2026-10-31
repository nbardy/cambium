#!/usr/bin/env python3
"""Measure real builds in disposable Git and Cambium worktrees.

Examples:
  python3 scripts/workflow-benchmark.py --fixture npm --count 2 \
    --cambium-bin "$PWD/bin/cambium" --output dist/workflow-npm.json
  python3 scripts/workflow-benchmark.py --recipe /path/to/recipe.json \
    --cambium-bin "$PWD/bin/cambium" --output dist/workflow-project.json

Recipe commands are trusted executable argv arrays, never shell snippets. They
run in private clones, but are not sandboxed. A recipe's source repository is
only read. {index} in edit text or command arguments is the workspace number.
The schema and all commands, edits, logs, versions and phase measurements are
retained in the report. A failing run exits nonzero and keeps its scratch tree.
"""

from __future__ import annotations

import argparse
import contextlib
import datetime as dt
import hashlib
import json
import math
import os
from pathlib import Path, PurePosixPath
import platform
import shutil
import signal
import statistics
import subprocess
import sys
import tempfile
import time
from typing import Any


METHODS = ("git-env-copy", "cambium-auto", "cambium-cow")
ROOT = Path(__file__).resolve().parent.parent


def utc_now() -> str:
    return dt.datetime.now(dt.timezone.utc).isoformat()


def write_json(path: Path, value: Any) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_name(path.name + ".tmp")
    with temporary.open("w", encoding="utf-8") as handle:
        json.dump(value, handle, indent=2, sort_keys=True)
        handle.write("\n")
        handle.flush()
        os.fsync(handle.fileno())
    os.replace(temporary, path)


def relative_path(value: str) -> PurePosixPath:
    path = PurePosixPath(value)
    if not value or path.is_absolute() or ".." in path.parts or ".git" in path.parts or "\x00" in value or "\\" in value or path == PurePosixPath("."):
        raise ValueError(f"expected a concrete repository-relative path, got {value!r}")
    return path


def owned_path(root: Path, relative: str) -> Path:
    path = root.joinpath(*relative_path(relative).parts)
    if not path.resolve().is_relative_to(root.resolve()):
        raise ValueError(f"path escapes disposable workspace through a symlink: {relative!r}")
    return path


def validate_recipe(value: Any) -> dict[str, Any]:
    if not isinstance(value, dict) or value.get("version") != 1:
        raise ValueError("recipe must be an object with version = 1")
    allowed = {"version", "name", "source", "ref", "environment_paths", "prepare", "verify", "source_edits", "dependency_edits", "dependency_prepare", "allow_policy_commands", "provenance"}
    if set(value) - allowed:
        raise ValueError(f"unknown recipe fields: {sorted(set(value) - allowed)}")
    for key in ("name", "source", "ref"):
        if not isinstance(value.get(key), str) or not value[key].strip():
            raise ValueError(f"recipe.{key} must be a nonempty string")
    paths = value.get("environment_paths")
    if not isinstance(paths, list) or not paths:
        raise ValueError("recipe.environment_paths must be a nonempty array")
    normalized = [relative_path(p) for p in paths]
    for i, path in enumerate(normalized):
        for other in normalized[i + 1:]:
            if path == other or path in other.parents or other in path.parents:
                raise ValueError(f"overlapping environment paths: {path} and {other}")
    for key in ("prepare", "verify", "dependency_prepare"):
        commands = value.get(key)
        if not isinstance(commands, list) or (key == "verify" and not commands):
            raise ValueError(f"recipe.{key} must be an array of argv arrays; verify cannot be empty")
        for command in commands:
            if not isinstance(command, list) or not command or any(not isinstance(arg, str) or "\x00" in arg for arg in command) or not command[0]:
                raise ValueError(f"recipe.{key} contains an invalid argv array")
    for key in ("source_edits", "dependency_edits"):
        edits = value.get(key)
        if not isinstance(edits, list) or not edits:
            raise ValueError(f"recipe.{key} must contain at least one checked replacement")
        for edit in edits:
            if not isinstance(edit, dict) or set(edit) - {"path", "old", "new", "count"}:
                raise ValueError(f"recipe.{key} contains an invalid edit")
            relative_path(edit.get("path", ""))
            if not isinstance(edit.get("old"), str) or not edit["old"] or not isinstance(edit.get("new"), str) or edit["old"] == edit["new"]:
                raise ValueError("each edit needs different old/new text and nonempty old text")
            count = edit.get("count", 1)
            if type(count) is not int or count < 1:
                raise ValueError("edit.count must be a positive integer")
    if type(value.get("allow_policy_commands", False)) is not bool:
        raise ValueError("allow_policy_commands must be boolean")
    return value


def expand(value: str, index: int) -> str:
    return value.replace("{index}", str(index))


def apply_edit(workspace: Path, edit: dict[str, Any], index: int) -> dict[str, Any]:
    path = owned_path(workspace, edit["path"])
    if path.is_symlink() or not path.is_file():
        raise ValueError(f"edit target must be a regular file, not a symlink: {edit['path']}")
    old, new = expand(edit["old"], index), expand(edit["new"], index)
    before = path.read_bytes()
    content = before.decode("utf-8")
    expected, actual = edit.get("count", 1), content.count(old)
    if actual != expected:
        raise ValueError(f"checked edit {edit['path']}: expected {expected} occurrences, found {actual}")
    after = content.replace(old, new).encode("utf-8")
    path.write_bytes(after)
    return {"path": edit["path"], "replacements": actual, "before_sha256": hashlib.sha256(before).hexdigest(), "after_sha256": hashlib.sha256(after).hexdigest()}


def tree_sizes(root: Path) -> dict[str, int]:
    logical = allocated = files = 0
    if root.exists():
        for directory, _, names in os.walk(root, followlinks=False):
            for name in names:
                try:
                    stat = (Path(directory) / name).lstat()
                except FileNotFoundError:
                    continue
                logical += stat.st_size
                allocated += getattr(stat, "st_blocks", 0) * 512
                files += 1
    # st_blocks counts shared extents in each reference: it is NOT CoW usage.
    return {"logical_file_bytes": logical, "referenced_allocated_file_bytes": allocated, "files": files}


def disk_point(root: Path, start_free: int | None = None, warm_free: int | None = None) -> dict[str, Any]:
    if hasattr(os, "sync"):
        os.sync()
    disk = os.statvfs(root)
    free = disk.f_bfree * disk.f_frsize
    result: dict[str, Any] = {"at": utc_now(), "volume_free_bytes": free, "volume_total_bytes": disk.f_blocks * disk.f_frsize, "tree": tree_sizes(root)}
    if start_free is not None:
        result["volume_used_delta_from_method_start_bytes"] = start_free - free
    if warm_free is not None:
        result["volume_used_delta_from_warm_reference_bytes"] = warm_free - free
    result["cambium_state"] = tree_sizes(root / "reference" / ".git" / "cambium")
    return result


class CommandFailure(RuntimeError):
    pass


class Recorder:
    def __init__(self, output: Path, report: dict[str, Any], timeout: float):
        self.output, self.report, self.timeout = output, report, timeout
        self.logs = Path(tempfile.mkdtemp(prefix=output.stem + "-logs-", dir=output.parent))
        self.env = dict(os.environ)
        # Benchmark commits/configuration must not pick up a user's Git hooks or
        # signing settings. Build commands still use the caller's toolchains.
        self.env.update({"GIT_CONFIG_GLOBAL": os.devnull, "GIT_CONFIG_NOSYSTEM": "1", "GIT_TERMINAL_PROMPT": "0"})
        self.env.pop("GIT_DIR", None)
        self.env.pop("GIT_WORK_TREE", None)
        self.env.pop("GIT_INDEX_FILE", None)
        self.checkpoint()

    def checkpoint(self) -> None:
        write_json(self.output, self.report)

    def run(self, argv: list[str], cwd: Path, phase: str, method: str = "setup", index: int | None = None, allowed: tuple[int, ...] = (0,)) -> str:
        commands = self.report.setdefault("commands", [])
        log = self.logs / f"{len(commands):04d}-{method}-{phase}.log"
        entry: dict[str, Any] = {"argv": argv, "cwd": str(cwd), "phase": phase, "method": method, "workspace_index": index, "started_at": utc_now(), "status": "running", "log": str(log.relative_to(self.output.parent))}
        commands.append(entry)
        self.checkpoint()
        start = time.monotonic()
        process = None
        try:
            with log.open("wb") as handle:
                process = subprocess.Popen(argv, cwd=cwd, env=self.env, stdout=handle, stderr=subprocess.STDOUT, start_new_session=True)
                try:
                    code = process.wait(timeout=self.timeout)
                except BaseException:
                    os.killpg(process.pid, signal.SIGTERM)
                    try:
                        process.wait(timeout=5)
                    except subprocess.TimeoutExpired:
                        os.killpg(process.pid, signal.SIGKILL)
                        process.wait()
                    raise
            entry["exit_code"] = code
            entry["status"] = "ok" if code in allowed else "failed"
        except BaseException as error:
            entry["status"] = "failed"
            entry["error"] = str(error)
            raise
        finally:
            entry["elapsed_seconds"] = time.monotonic() - start
            self.checkpoint()
        content = log.read_text(encoding="utf-8", errors="replace")
        if code not in allowed:
            raise CommandFailure(f"{method}/{phase}: {argv!r} exited {code}; log: {log}\n{content[-4000:]}")
        return content


def capture_version(argv: list[str], env: dict[str, str]) -> dict[str, Any]:
    try:
        result = subprocess.run(argv, env=env, capture_output=True, text=True, timeout=15)
        return {"argv": argv, "exit_code": result.returncode, "output": (result.stdout + result.stderr).strip()}
    except (OSError, subprocess.TimeoutExpired) as error:
        return {"argv": argv, "unavailable": str(error)}


def host_info(root: Path, recorder: Recorder, cambium: str) -> dict[str, Any]:
    info: dict[str, Any] = {"platform": platform.platform(), "system": platform.system(), "machine": platform.machine(), "processor": platform.processor(), "cpu_count": os.cpu_count(), "python": sys.version, "scratch_device": root.stat().st_dev}
    try:
        info["memory_bytes"] = os.sysconf("SC_PAGE_SIZE") * os.sysconf("SC_PHYS_PAGES")
    except (ValueError, OSError):
        pass
    info["versions"] = {name: capture_version([name, "--version"], recorder.env) for name in ("git", "node", "npm", "cargo", "rustc")}
    info["versions"]["cambium"] = capture_version([cambium, "version"], recorder.env)
    if Path(cambium).is_file():
        info["cambium_binary_sha256"] = hashlib.sha256(Path(cambium).read_bytes()).hexdigest()
    info["benchmark_script_sha256"] = hashlib.sha256(Path(__file__).read_bytes()).hexdigest()
    info["filesystem"] = capture_version(["stat", "-f", "%T", str(root)] if sys.platform == "darwin" else ["stat", "-f", "-c", "%T", str(root)], recorder.env)
    if sys.platform == "darwin":
        info["hardware"] = capture_version(["sysctl", "hw.model", "hw.memsize", "machdep.cpu.brand_string"], recorder.env)
    elif sys.platform.startswith("linux"):
        info["hardware"] = capture_version(["lscpu"], recorder.env)
    info["external_tool_caches"] = {key: os.environ[key] for key in ("CARGO_HOME", "RUSTUP_HOME", "npm_config_cache", "NPM_CONFIG_CACHE") if key in os.environ}
    return info


def fixture_recipe(kind: str, root: Path, recorder: Recorder) -> dict[str, Any]:
    fixture = ROOT / "scripts" / "workflow-fixtures" / kind
    source = root / ("fixture-" + kind)
    shutil.copytree(fixture, source)
    recipe = json.loads((source / "recipe.json").read_text())
    (source / "recipe.json").unlink()
    recipe["source"] = str(source)
    recipe["ref"] = "HEAD"
    recorder.run(["git", "init", "-q", "-b", "main"], source, "fixture")
    recorder.run(["git", "config", "user.name", "Cambium Workflow Benchmark"], source, "fixture")
    recorder.run(["git", "config", "user.email", "benchmark@example.invalid"], source, "fixture")
    lock_command = ["npm", "install", "--package-lock-only", "--offline", "--ignore-scripts", "--no-audit", "--no-fund"] if kind == "npm" else ["cargo", "generate-lockfile", "--offline"]
    recorder.run(lock_command, source, "fixture")
    recorder.run(["git", "add", "."], source, "fixture")
    recorder.run(["git", "commit", "-q", "-m", "Reproducible offline workflow fixture"], source, "fixture")
    return validate_recipe(recipe)


def copy_environment(recorder: Recorder, source: Path, target: Path, path: str, method: str, index: int) -> None:
    src, dst = owned_path(source, path), owned_path(target, path)
    if src.is_symlink() or not src.exists():
        raise ValueError(f"reference environment output is missing or a symlink: {path}")
    if dst.exists() or dst.is_symlink():
        raise ValueError(f"environment destination already exists: {path}")
    dst.parent.mkdir(parents=True, exist_ok=True)
    argv = ["cp", "-p", "-R", str(src), str(dst)] if sys.platform == "darwin" else ["cp", "--reflink=never", "-a", str(src), str(dst)]
    recorder.run(argv, source, "create", method, index)


def run_commands(recorder: Recorder, commands: list[list[str]], cwd: Path, phase: str, method: str, index: int = 0) -> None:
    for argv in commands:
        recorder.run([expand(arg, index) for arg in argv], cwd, phase, method, index)


def verify_environment_paths(recorder: Recorder, recipe: dict[str, Any], reference: Path, method: str) -> None:
    for path in recipe["environment_paths"]:
        output = owned_path(reference, path)
        if not output.exists() or output.is_symlink():
            raise ValueError(f"prepare must produce a regular environment output at {path}")
        tracked = recorder.run(["git", "ls-files", "--", path], reference, "prepare", method)
        if tracked.strip():
            raise ValueError(f"environment output contains tracked files: {path}")
        recorder.run(["git", "check-ignore", "-q", "--", path], reference, "prepare", method)


def apply_edits(recorder: Recorder, workspace: Path, edits: list[dict[str, Any]], phase: str, method: str, index: int) -> list[dict[str, Any]]:
    results = []
    for edit in edits:
        recorder.run(["git", "ls-files", "--error-unmatch", "--", edit["path"]], workspace, phase, method, index)
        results.append(apply_edit(workspace, edit, index))
    return results


def benchmark_method(recorder: Recorder, recipe: dict[str, Any], source: Path, source_commit: str, root: Path, method: str, count: int, cambium: str) -> None:
    method_root = root / method
    method_root.mkdir()
    result: dict[str, Any] = {"method": method, "status": "running", "workspace_count": count, "phases": [], "workspaces": []}
    recorder.report["results"].append(result)
    first = disk_point(method_root)
    result["before_clone"] = first
    start_free = first["volume_free_bytes"]
    warm_free = None
    reference = method_root / "reference"

    @contextlib.contextmanager
    def phase(name: str):
        print(f"{recipe['name']}: {method}: {name}", flush=True)
        stage: dict[str, Any] = {"name": name, "started_at": utc_now(), "status": "running"}
        result["phases"].append(stage)
        recorder.checkpoint()
        started = time.monotonic()
        try:
            yield stage
            stage["status"] = "ok"
        except BaseException as error:
            stage["status"] = "failed"
            stage["error"] = str(error)
            raise
        finally:
            stage["elapsed_seconds"] = time.monotonic() - started
            stage["measurement"] = disk_point(method_root, start_free, warm_free)
            recorder.checkpoint()

    try:
        with phase("prepare_reference"):
            recorder.run(["git", "clone", "--quiet", "--no-local", "--no-checkout", str(source), str(reference)], method_root, "prepare", method)
            recorder.run(["git", "checkout", "--quiet", "--detach", source_commit], reference, "prepare", method)
            recorder.run(["git", "config", "user.name", "Cambium Workflow Benchmark"], reference, "prepare", method)
            recorder.run(["git", "config", "user.email", "benchmark@example.invalid"], reference, "prepare", method)
            run_commands(recorder, recipe["prepare"], reference, "prepare", method)
            run_commands(recorder, recipe["verify"], reference, "reference_verify", method)
            status = recorder.run(["git", "status", "--porcelain", "--untracked-files=normal"], reference, "prepare", method)
            if status.strip():
                raise ValueError("prepare/verify changed reference source; commit generated lockfiles in the benchmark source first:\n" + status)
            verify_environment_paths(recorder, recipe, reference, method)
            if method.startswith("cambium") and recipe.get("allow_policy_commands", False):
                recorder.run([cambium, "init", "--allow-policy-commands"], reference, "prepare", method)
        warm_free = result["phases"][-1]["measurement"]["volume_free_bytes"]
        result["warm_reference_volume_free_bytes"] = warm_free

        with phase("create") as stage:
            durations = []
            for index in range(1, count + 1):
                name = f"workflow-{index:04d}"
                path = method_root / "worktrees" / name
                path.parent.mkdir(exist_ok=True)
                workspace: dict[str, Any] = {"index": index, "name": name, "path": str(path), "branch": name}
                result["workspaces"].append(workspace)
                began = time.monotonic()
                if method == "git-env-copy":
                    recorder.run(["git", "worktree", "add", "-b", name, str(path), source_commit], reference, "create", method, index)
                    for output in recipe["environment_paths"]:
                        copy_environment(recorder, reference, path, output, method, index)
                else:
                    output = recorder.run([cambium, "add", "--json", "--materializer", method.removeprefix("cambium-"), "--name", name, "-b", name, str(path), source_commit], reference, "create", method, index)
                    workspace["cambium_creation"] = json.loads(output)
                duration = time.monotonic() - began
                workspace["create_seconds"] = duration
                durations.append(duration)
            stage["create_p50_seconds"] = statistics.median(durations)
            stage["create_p95_seconds"] = sorted(durations)[math.ceil(len(durations) * 0.95) - 1]

        for phase_name, edit_key in (("initial_build_test", None), ("source_edit_build_test", "source_edits"), ("dependency_edit_build_test", "dependency_edits")):
            with phase(phase_name) as stage:
                stage["workspaces"] = []
                for workspace in result["workspaces"]:
                    path, index = Path(workspace["path"]), workspace["index"]
                    entry: dict[str, Any] = {"index": index, "verification_passed": False}
                    stage["workspaces"].append(entry)
                    started = time.monotonic()
                    if edit_key:
                        entry["edits"] = apply_edits(recorder, path, recipe[edit_key], phase_name, method, index)
                    if edit_key == "dependency_edits":
                        run_commands(recorder, recipe["dependency_prepare"], path, phase_name, method, index)
                    run_commands(recorder, recipe["verify"], path, phase_name, method, index)
                    entry["elapsed_seconds"] = time.monotonic() - started
                    entry["verification_passed"] = True
                    entry["git_status"] = recorder.run(["git", "status", "--porcelain"], path, phase_name, method, index)
                stage["verified_workspaces"] = count

        with phase("remove_worktrees"):
            for workspace in result["workspaces"]:
                path, name = Path(workspace["path"]), workspace["name"]
                if not path.resolve().is_relative_to(method_root.resolve()):
                    raise ValueError("refusing cleanup outside the owned benchmark root")
                if method == "git-env-copy":
                    recorder.run(["git", "worktree", "remove", "--force", str(path)], reference, "remove", method)
                    recorder.run(["git", "branch", "-D", name], reference, "remove", method)
                else:
                    recorder.run([cambium, "remove", "--force", "--delete-branch", name], reference, "remove", method)
                if path.exists():
                    raise RuntimeError(f"worktree survived removal: {path}")

        with phase("prune") as stage:
            if method.startswith("cambium"):
                stage["prune_result"] = json.loads(recorder.run([cambium, "prune", "--older-than", "1ns", "--json"], reference, "prune", method))
            recorder.run(["git", "worktree", "prune"], reference, "prune", method)
            registrations = recorder.run(["git", "worktree", "list", "--porcelain"], reference, "prune", method)
            if sum(line.startswith("worktree ") for line in registrations.splitlines()) != 1:
                raise RuntimeError("cleanup left extra registered worktrees")
        result["status"] = "ok"
    except BaseException as error:
        result["status"] = "failed"
        result["error"] = str(error)
        raise
    finally:
        recorder.checkpoint()


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    source = parser.add_mutually_exclusive_group(required=True)
    source.add_argument("--fixture", choices=("npm", "cargo"))
    source.add_argument("--recipe", type=Path)
    parser.add_argument("--cambium-bin", default=str(ROOT / "bin" / "cambium"))
    parser.add_argument("--methods", default="git-env-copy,cambium-auto")
    parser.add_argument("--count", type=int, default=5)
    parser.add_argument("--output", required=True, type=Path)
    parser.add_argument("--scratch", type=Path, help="existing parent for a new, owned temporary directory")
    parser.add_argument("--keep", action="store_true", help="retain successful scratch data; failures always retain it")
    parser.add_argument("--command-timeout", type=float, default=1800)
    parser.add_argument("--overwrite", action="store_true")
    args = parser.parse_args(argv)
    if args.count < 1 or args.command_timeout <= 0:
        parser.error("--count and --command-timeout must be positive")
    methods = args.methods.split(",")
    if not methods or len(set(methods)) != len(methods) or any(method not in METHODS for method in methods):
        parser.error(f"--methods must contain unique values from {','.join(METHODS)}")
    output = args.output.resolve()
    if output.exists() and not args.overwrite:
        parser.error(f"report already exists: {output}; choose a new path or pass --overwrite")
    output.parent.mkdir(parents=True, exist_ok=True)
    cambium = shutil.which(args.cambium_bin)
    if any(method.startswith("cambium") for method in methods) and not cambium:
        parser.error(f"Cambium binary is not executable: {args.cambium_bin}")
    cambium = cambium or args.cambium_bin
    if args.scratch:
        args.scratch.mkdir(parents=True, exist_ok=True)
    root = Path(tempfile.mkdtemp(prefix="cambium-workflow-", dir=args.scratch)).resolve()
    report: dict[str, Any] = {"format_version": 1, "started_at": utc_now(), "status": "running", "scratch": str(root), "method_order": methods, "results": [], "measurement_notes": [
        "Every method starts with a separately cloned, built and tested reference at the same recorded commit. Prepare costs are reported separately from warm-workspace overhead.",
        "Physical figures are signed filesystem free-space deltas, including Cambium baselines/layers retained in .git. Ambient activity and same-volume tool-cache writes can affect them; negative deltas are not hidden.",
        "Logical and st_blocks-derived tree sizes are diagnostic only: shared CoW extents can be counted repeatedly and these sizes do not establish physical savings.",
        "Toolchain/package-manager global caches are shared and warmed during each reference preparation; their external paths are recorded when explicitly configured. Caches on other volumes are outside these physical measurements.",
        "Initial, source-edit and dependency-edit verification runs actual recipe build/test commands. Cambium's own readiness metadata is recorded separately and is not treated as proof of runnable state.",
        "Workspaces are tested sequentially. This harness measures space, relocation and incremental rebuild behavior; it does not claim concurrent throughput or statistical significance.",
    ]}
    recorder = Recorder(output, report, args.command_timeout)
    success = False
    try:
        report["host"] = host_info(root, recorder, cambium)
        recipe = fixture_recipe(args.fixture, root, recorder) if args.fixture else validate_recipe(json.loads(args.recipe.read_text()))
        source_root = Path(recipe["source"]).expanduser().resolve()
        if not source_root.is_dir():
            raise ValueError(f"source directory does not exist: {source_root}")
        commit = recorder.run(["git", "rev-parse", "--verify", recipe["ref"] + "^{commit}"], source_root, "resolve").strip()
        tree = recorder.run(["git", "rev-parse", "--verify", commit + "^{tree}"], source_root, "resolve").strip()
        origin = recorder.run(["git", "config", "--get", "remote.origin.url"], source_root, "resolve", allowed=(0, 1)).strip()
        report.update({"recipe": recipe, "resolved_source_commit": commit, "resolved_source_tree": tree, "source_origin": origin})
        recorder.checkpoint()
        for method in methods:
            benchmark_method(recorder, recipe, source_root, commit, root, method, args.count, cambium)
        report["status"] = "ok"
        success = True
    except (Exception, KeyboardInterrupt) as error:
        report["status"] = "failed"
        report["error"] = str(error) or type(error).__name__
        print(f"Workflow benchmark failed: {report['error']}", file=sys.stderr)
    finally:
        report["completed_at"] = utc_now()
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
