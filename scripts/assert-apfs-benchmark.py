#!/usr/bin/env python3
"""Validate the APFS invariants Cambium claims, without pretending timing is stable."""

from __future__ import annotations

import json
import sys
from pathlib import Path


def fail(message: str) -> None:
    raise SystemExit(f"APFS benchmark assertion failed: {message}")


def main() -> None:
    if len(sys.argv) != 2:
        fail("usage: assert-apfs-benchmark.py REPORT.json")
    report = json.loads(Path(sys.argv[1]).read_text())
    filesystem = str(report.get("filesystem", "")).lower()
    if "apfs" not in filesystem:
        fail(f"benchmark did not run on APFS: {filesystem!r}")
    results = {row["method"]: row for row in report.get("results", [])}
    for method in ("git-env-copy", "cambium-cow"):
        if method not in results:
            fail(f"missing method {method}")
        if not results[method].get("available"):
            fail(f"{method} unavailable: {results[method].get('skip_reason', '')}")
    control = results["git-env-copy"]
    cambium = results["cambium-cow"]
    count = int(cambium.get("workspace_count", 0))
    if cambium.get("clone_mode") != "apfs-clone":
        fail(f"wrong clone mode: {cambium.get('clone_mode')!r}")
    if int(cambium.get("environment_ready", 0)) != count:
        fail("Cambium workspaces were not all environment-ready")
    if int(control.get("environment_ready", 0)) != count:
        fail("Git fairness-control workspaces were not all environment-ready")
    control_bytes = int(control.get("physical_added_bytes", 0))
    cambium_bytes = int(cambium.get("physical_added_bytes", 0))
    if control_bytes <= 0 or cambium_bytes <= 0:
        fail(f"invalid allocation measurements: control={control_bytes}, cambium={cambium_bytes}")
    # The synthetic workload is deliberately large enough that APFS metadata
    # noise is tiny relative to eight ordinary copies. This threshold is broad:
    # it proves structural sharing without treating CI timing as a product SLA.
    if cambium_bytes * 2 >= control_bytes:
        fail(
            "Cambium did not use less than half the physical space of the "
            f"same-semantics control ({cambium_bytes} vs {control_bytes})"
        )
    ratio = cambium_bytes / control_bytes
    print(
        f"APFS CoW verified: Cambium used {cambium_bytes} bytes vs "
        f"{control_bytes} bytes ({ratio:.1%}) for {count} ready workspaces"
    )


if __name__ == "__main__":
    main()
