#!/usr/bin/env python3
"""Safety and failure-reporting tests for the real-workflow benchmark harness."""

import contextlib
import importlib.util
import io
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest


SCRIPT = Path(__file__).with_name("workflow-benchmark.py")
SPEC = importlib.util.spec_from_file_location("workflow_benchmark", SCRIPT)
benchmark = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(benchmark)


class WorkflowBenchmarkSafetyTest(unittest.TestCase):
    def test_invalid_prune_policy_does_not_create_output(self):
        for flags in (("--prune-max-unused-bytes", "-1"), ("--prune-max-unused-bytes", "0", "--prune-older-than", "1ns")):
            with self.subTest(flags=flags), tempfile.TemporaryDirectory() as temporary:
                output = Path(temporary) / "result.json"
                with contextlib.redirect_stderr(io.StringIO()), self.assertRaises(SystemExit):
                    benchmark.main(["--fixture", "npm", "--output", str(output), *flags])
                self.assertFalse(output.exists())

    def test_edit_refuses_traversal_and_git_administration(self):
        for path in ("../outside", "/outside", "a/../../outside", ".git/config", "nested/.git/index", ".", "a\\..\\outside"):
            with self.subTest(path=path), self.assertRaises(ValueError):
                benchmark.relative_path(path)

    def test_edit_refuses_symlink_escape_without_touching_target(self):
        with tempfile.TemporaryDirectory() as temporary:
            base = Path(temporary)
            root = base / "workspace"
            root.mkdir()
            outside = base / "important.txt"
            outside.write_text("keep this")
            (root / "link").symlink_to(base, target_is_directory=True)
            with self.assertRaises(ValueError):
                benchmark.apply_edit(root, {"path": "link/important.txt", "old": "keep", "new": "lose"}, 1)
            self.assertEqual(outside.read_text(), "keep this")

    def test_failed_exact_replacement_does_not_write_partial_edit(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            file = root / "source.js"
            file.write_text("old old")
            with self.assertRaisesRegex(ValueError, "found 2"):
                benchmark.apply_edit(root, {"path": "source.js", "old": "old", "new": "new"}, 1)
            self.assertEqual(file.read_text(), "old old")

    def test_edit_preserves_mode_and_non_template_braces(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            file = root / "run.py"
            file.write_text("print({'name': 'baseline'})\n")
            file.chmod(0o755)
            benchmark.apply_edit(root, {"path": "run.py", "old": "baseline", "new": "agent-{index}"}, 7)
            self.assertEqual(file.read_text(), "print({'name': 'agent-7'})\n")
            self.assertEqual(file.stat().st_mode & 0o777, 0o755)

    def test_failed_command_retains_report_and_complete_output(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            output = root / "report.json"
            recorder = benchmark.Recorder(output, {"status": "running"}, timeout=10)
            with self.assertRaises(benchmark.CommandFailure):
                recorder.run([sys.executable, "-c", "print('failure evidence'); raise SystemExit(9)"], root, "test")
            report = json.loads(output.read_text())
            command = report["commands"][0]
            self.assertEqual(command["exit_code"], 9)
            self.assertEqual(command["status"], "failed")
            self.assertIn("failure evidence", (root / command["log"]).read_text())

    def test_full_git_workflow_preserves_source_repository(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            source = root / "source"
            source.mkdir()
            (source / ".gitignore").write_text("deps/\n")
            (source / "source.txt").write_text("baseline\n")
            (source / "dependency.lock").write_text("dependency-v1\n")
            for command in (
                ["git", "init", "-q", "-b", "main"],
                ["git", "config", "user.name", "Benchmark Test"],
                ["git", "config", "user.email", "test@example.invalid"],
                ["git", "add", "."],
                ["git", "commit", "-q", "-m", "fixture"],
            ):
                subprocess.run(command, cwd=source, check=True, capture_output=True)
            before = subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=source)
            recipe = {
                "version": 1, "name": "source-isolation", "source": str(source), "ref": "HEAD",
                "environment_paths": ["deps"],
                "prepare": [[sys.executable, "-c", "from pathlib import Path; Path('deps').mkdir(); Path('deps/input').write_text('prepared')"]],
                "verify": [[sys.executable, "-c", "from pathlib import Path; assert Path('deps/input').read_text() == 'prepared'"]],
                "source_edits": [{"path": "source.txt", "old": "baseline", "new": "agent-{index}"}],
                "dependency_edits": [{"path": "dependency.lock", "old": "dependency-v1", "new": "dependency-v2"}],
                "dependency_prepare": [],
            }
            recipe_path = root / "recipe.json"
            recipe_path.write_text(json.dumps(recipe))
            output = root / "result.json"
            with contextlib.redirect_stdout(io.StringIO()):
                code = benchmark.main(["--recipe", str(recipe_path), "--methods", "git-env-copy", "--count", "2", "--output", str(output), "--scratch", str(root)])
            self.assertEqual(code, 0, output.read_text())
            report = json.loads(output.read_text())
            self.assertEqual(report["status"], "ok")
            self.assertEqual(report["prune_policy"], {"older_than": "1ns"})
            self.assertEqual(subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=source), before)
            self.assertEqual(subprocess.check_output(["git", "status", "--porcelain"], cwd=source), b"")
            self.assertEqual((source / "source.txt").read_text(), "baseline\n")
            self.assertEqual((source / "dependency.lock").read_text(), "dependency-v1\n")
            self.assertFalse((source / "deps").exists())
            self.assertFalse(Path(report["scratch"]).exists())
            phases = {phase["name"]: phase for phase in report["results"][0]["phases"]}
            for name in ("initial_build_test", "source_edit_build_test", "dependency_edit_build_test"):
                self.assertEqual(phases[name]["verified_workspaces"], 2)
            self.assertEqual(phases["prune"]["status"], "ok")


if __name__ == "__main__":
    unittest.main()
