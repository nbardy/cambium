#!/usr/bin/env python3
"""Compare two Cambium binaries on unverified environment operations.

Creates only disposable repositories. Measures actual CLI commands, checks
their results agree, and counts Git subprocesses separately using Trace2.
"""

from __future__ import annotations

import argparse
import datetime as dt
import hashlib
import json
import os
from pathlib import Path
import platform
import statistics
import subprocess
import tempfile
import time


def command_environment():
    # Ambient repository overrides must not redirect this disposable fixture's
    # Git commands into the caller's working repository or object database.
    return {key: value for key, value in os.environ.items() if not key.startswith("GIT_")}


def run(argv, cwd, expected=0, env=None):
    result = subprocess.run(argv, cwd=cwd, env=command_environment() if env is None else env,
                            capture_output=True, text=True)
    if result.returncode != expected:
        raise RuntimeError(f"{argv!r} exited {result.returncode}: {result.stderr}")
    return result.stdout


def free_bytes(path):
    os.sync()
    value = os.statvfs(path)
    return value.f_bfree * value.f_frsize


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--before-bin", type=Path, required=True)
    parser.add_argument("--after-bin", type=Path, required=True)
    parser.add_argument("--files", type=int, default=10000)
    parser.add_argument("--layers", type=int, default=8)
    parser.add_argument("--repetitions", type=int, default=7)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    if min(args.files, args.layers, args.repetitions) < 1:
        parser.error("files, layers, and repetitions must be positive")
    if args.output.exists():
        parser.error("output already exists; choose a new path")
    binaries = {"before": args.before_bin.resolve(), "after": args.after_bin.resolve()}
    for binary in binaries.values():
        if not binary.is_file():
            parser.error(f"binary does not exist: {binary}")

    report = {
        "version": 1,
        "started_at": dt.datetime.now(dt.timezone.utc).isoformat(),
        "host": {"platform": platform.platform(), "cpu_count": os.cpu_count()},
        "fixture": {"tracked_source_files": args.files, "source_bytes_per_file": 1024,
                    "present_unvalidated_layers": args.layers, "missing_required_layers": 1},
        "repetitions": args.repetitions,
        "binaries": {label: {"path": str(path), "sha256": hashlib.sha256(path.read_bytes()).hexdigest()}
                     for label, path in binaries.items()},
        "results": {},
        "measurement_notes": [
            "One disposable Git repository and warmed environment cache shared by both binaries.",
            "Before/after order alternates per repetition. Warmup and cleanup are outside timings.",
            "Git Trace2 counts are separate untimed runs; timed runs do not enable tracing.",
            "Signed free-space deltas are synchronized whole-volume observations; unrelated writes can affect them.",
            "This measures unverified/missing paths, not configured successful validators or dependency payload throughput."
        ],
    }
    expected_unverified = [f"deps-{i:02d}" for i in range(args.layers)]

    with tempfile.TemporaryDirectory(prefix="cambium-observation-bench-") as tmp:
        scratch = Path(tmp)
        root = scratch / "source"
        root.mkdir()
        run(["git", "init", "-q"], root)
        run(["git", "config", "user.name", "Cambium benchmark"], root)
        run(["git", "config", "user.email", "benchmark@example.invalid"], root)
        run(["git", "config", "core.hooksPath", os.devnull], root)
        (root / ".gitignore").write_text("deps-*\nmissing-deps\n")
        (root / "deps.lock").write_text("version=1\n")
        policy = ["version = 1\n"]
        for name in expected_unverified:
            (root / name).mkdir()
            (root / name / "installed").write_text("unverified installation\n")
            policy.append(f'[[path]]\npath = "{name}"\npolicy = "clone"\ninputs = ["deps.lock"]\nrequired = true\n')
        policy.append('[[path]]\npath = "missing-deps"\npolicy = "clone"\ninputs = ["deps.lock"]\nrequired = true\nvalidate = ["false"]\n')
        (root / ".cambium.toml").write_text("\n".join(policy))
        for i in range(args.files):
            path = root / "src" / f"{i // 100:05d}" / f"{i:07d}.txt"
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_bytes(b"x" * 1023 + b"\n")
        run(["git", "add", "."], root)
        run(["git", "commit", "-qm", "benchmark fixture"], root)
        report["fixture"]["commit"] = run(["git", "rev-parse", "HEAD"], root).strip()
        report["host"]["git"] = run(["git", "--version"], root).strip()
        run([str(binaries["before"]), "init", "--allow-policy-commands"], root)
        run([str(binaries["before"]), "create", "--materializer", "git", "--json", "baseline"], root)

        def invoke(label, operation, name, trace=False):
            binary = str(binaries[label])
            if operation == "create":
                argv = [binary, "create", "--materializer", "git", "--json", name]
                expected = 0
            else:
                argv = [binary, "env", operation, "--json", "baseline"]
                expected = 1 if operation == "validate" else 0
            env = command_environment()
            trace_path = scratch / f"trace-{name}.jsonl"
            if trace:
                env["GIT_TRACE2_EVENT"] = str(trace_path)
            before_free = free_bytes(scratch)
            start = time.perf_counter()
            value = json.loads(run(argv, root, expected=expected, env=env))
            elapsed = time.perf_counter() - start
            after_free = free_bytes(scratch)
            prefix = "environment_" if operation == "create" else ""
            assert value[prefix + "ready"] is False, value
            assert sorted(value[prefix + "missing"]) == ["missing-deps"], value
            assert sorted(value[prefix + "unverified"]) == expected_unverified, value
            if operation == "create":
                run([binary, "remove", "--force", "--delete-branch", name], root)
            commands = []
            if trace:
                for line in trace_path.read_text().splitlines():
                    event = json.loads(line)
                    if event.get("event") == "start":
                        commands.append(event["argv"])
            return {"elapsed_seconds": elapsed, "volume_used_delta_bytes": before_free - after_free}, value, commands

        for operation in ["explain", "validate", "create"]:
            values = {label: {"samples": []} for label in binaries}
            observed = {}
            for label in binaries:
                _, value, commands = invoke(label, operation, f"trace-{operation}-{label}", trace=True)
                values[label]["git_subprocess_count"] = len(commands)
                values[label]["git_commands"] = commands
                if operation != "create":
                    observed[label] = value
            if observed:
                assert observed["before"] == observed["after"], (operation, observed)
            for i in range(args.repetitions):
                order = ["before", "after"] if i % 2 == 0 else ["after", "before"]
                for label in order:
                    sample, value, _ = invoke(label, operation, f"sample-{operation}-{label}-{i}")
                    values[label]["samples"].append(sample)
                    if operation != "create":
                        assert value == observed[label], (operation, value)
            for value in values.values():
                samples = [sample["elapsed_seconds"] for sample in value["samples"]]
                value["median_seconds"] = statistics.median(samples)
                value["min_seconds"] = min(samples)
                value["max_seconds"] = max(samples)
            report["results"][operation] = values
            print(operation, "; ".join(f'{label}: {value["median_seconds"]:.6f}s, {value["git_subprocess_count"]} Git processes' for label, value in values.items()), flush=True)
        report["completed_at"] = dt.datetime.now(dt.timezone.utc).isoformat()
        report["status"] = "ok"
    args.output.parent.mkdir(parents=True, exist_ok=True)
    with args.output.open("x") as output:
        json.dump(report, output, indent=2, sort_keys=True)
        output.write("\n")


if __name__ == "__main__":
    main()
