#!/usr/bin/env python3
"""Create a pinned external benchmark source and recipe in new paths.

Downloads are completed before measurement. This prepares a new disposable
checkout; it refuses an existing destination or recipe. Builds/tests execute
upstream project code, so use only the pinned projects you intend to trust.
"""

import argparse
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile


ROOT = Path(__file__).resolve().parent.parent
PROJECTS = {
    "clsx": ("https://github.com/lukeed/clsx.git", "925494cf31bcd97d3337aacd34e659e80cae7fe2"),
    "ripgrep": ("https://github.com/BurntSushi/ripgrep.git", "91f5d22ef48bde459d90a8c7dbeab416e97d0f2b"),
}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--project", choices=PROJECTS, required=True)
    parser.add_argument("--destination", type=Path, required=True)
    parser.add_argument("--recipe", type=Path, required=True)
    args = parser.parse_args()
    destination, output = args.destination.resolve(), args.recipe.resolve()
    if destination.exists() or output.exists():
        parser.error("destination and recipe must both be new paths")
    if output.is_relative_to(destination):
        parser.error("write the recipe outside the source checkout")
    env = dict(os.environ)
    env.update({"GIT_CONFIG_GLOBAL": os.devnull, "GIT_CONFIG_NOSYSTEM": "1", "GIT_TERMINAL_PROMPT": "0"})
    for key in ("GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE"):
        env.pop(key, None)

    def run(argv, cwd=destination, capture=False):
        result = subprocess.run(argv, cwd=cwd, env=env, text=True, check=True,
                                stdout=subprocess.PIPE if capture else None)
        return result.stdout.strip() if capture else None

    if args.project == "clsx":
        node = run(["node", "--version"], cwd=ROOT, capture=True)
        if not node.startswith("v18."):
            parser.error("the pinned clsx suite uses the legacy esm loader; use Node 18 (tested with 18.20.8)")
    destination.mkdir(parents=True)
    url, revision = PROJECTS[args.project]
    run(["git", "init", "-q", "-b", "benchmark-source"])
    run(["git", "remote", "add", "origin", url])
    run(["git", "fetch", "--quiet", "--depth", "1", "origin", revision])
    run(["git", "checkout", "--quiet", "--detach", revision])
    recipe = {
        "version": 1, "name": args.project, "source": str(destination),
        "ref": revision, "allow_policy_commands": False,
        "provenance": {"upstream_url": url, "upstream_commit": revision},
    }
    if args.project == "clsx":
        shutil.copyfile(ROOT / "scripts/workflow-projects/clsx.package-lock.json", destination / "package-lock.json")
        run(["git", "add", "-f", "package-lock.json"])
        env.update({"GIT_AUTHOR_DATE": "2026-10-30T00:00:00+00:00", "GIT_COMMITTER_DATE": "2026-10-30T00:00:00+00:00"})
        run(["git", "-c", "user.name=Cambium Benchmark", "-c", "user.email=benchmark@example.invalid",
             "-c", "commit.gpgsign=false", "commit", "-qm", "Pin npm dependencies for the Cambium workflow benchmark"])
        recipe["ref"] = run(["git", "rev-parse", "HEAD"], capture=True)
        npm_flags = ["--ignore-scripts", "--no-audit", "--no-fund"]
        run(["npm", "ci", *npm_flags])
        # Warm the alternate test runner and its dependencies outside the measured
        # source so offline dependency edits do not include registry downloads.
        with tempfile.TemporaryDirectory(prefix="cambium-clsx-dependencies-") as temporary:
            package = json.loads((destination / "package.json").read_text())
            package["devDependencies"]["uvu"] = "0.5.6"
            (Path(temporary) / "package.json").write_text(json.dumps(package, indent=2) + "\n")
            run(["npm", "install", *npm_flags], cwd=temporary)
        recipe.update({
            "environment_paths": ["node_modules", "dist"],
            "prepare": [["npm", "ci", "--offline", *npm_flags], ["npm", "run", "build"]],
            "verify": [["npm", "run", "build"], ["npm", "test"]],
            "source_edits": [{"path": "src/index.js", "old": "function toVal(mix) {", "new": "// Workspace {index} source edit.\nfunction toVal(mix) {"}],
            "dependency_edits": [{"path": "package.json", "old": '"uvu": "0.5.4"', "new": '"uvu": "0.5.6"'}],
            "dependency_prepare": [["npm", "install", "--offline", *npm_flags]],
        })
        recipe["provenance"]["description"] = "Upstream clsx plus a pinned lockfile; Node 18 legacy test loader, all 32 upstream tests. Source comment edit and uvu 0.5.4 to 0.5.6 test-runner change. Small library, not a monorepo-scale claim."
    else:
        run(["cargo", "fetch", "--locked"])
        recipe.update({
            "environment_paths": ["target"],
            "prepare": [["cargo", "build", "--offline", "--locked"]],
            "verify": [["cargo", "build", "--offline", "--locked"], ["cargo", "test", "--offline", "--locked", "--workspace", "--exclude", "grep-pcre2", "--lib", "--quiet"]],
            "source_edits": [{"path": "crates/core/main.rs", "old": "The main entry point into ripgrep.", "new": "The main entry point into ripgrep. Workspace {index} source edit."}],
            "dependency_edits": [{"path": "crates/searcher/Cargo.toml", "old": 'version = "0.1.17"  #:version', "new": 'version = "0.1.18"  #:version'}],
            "dependency_prepare": [["cargo", "build", "--offline"]],
        })
        recipe["provenance"]["description"] = "Pinned ripgrep source; actual rg build and workspace library tests excluding optional PCRE2. Source comment edit and local grep-searcher dependency version bump. CLI integration tests are outside this recipe: f1414_no_require_git failed in the review host before any Cambium use."
    output.parent.mkdir(parents=True, exist_ok=True)
    output.write_text(json.dumps(recipe, indent=2) + "\n")
    print(f"Prepared {args.project}: {recipe['ref']}\nRecipe: {output}")


if __name__ == "__main__":
    main()
