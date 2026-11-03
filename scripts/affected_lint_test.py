#!/usr/bin/env python3
"""Exercise lint scoping with real Git repositories, Go graphs and findings.

The portable analyzer is go vet (also enabled by golangci-lint). Set
LINT_SCOPE_TEST_LINTER to a golangci-lint binary to repeat the same proofs
against the complete lint command.
"""

import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest


ROOT = Path(__file__).resolve().parent.parent


class AffectedLintTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.cache = tempfile.TemporaryDirectory(dir=os.environ.get("TEST_TMPDIR"))
        cls.addClassCleanup(cls.cache.cleanup)

    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(dir=self.cache.name)
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.repo = self.root / "repo"
        self.repo.mkdir()
        self.log = self.root / "calls.jsonl"
        self.env = {key: value for key, value in os.environ.items()
                    if not key.startswith(("GIT_", "GO", "LINT_", "MAKE"))
                    and key not in ("HOME", "PYTHONPATH")}
        self.env.update(
            HOME=str(self.root), GOENV="off", GOWORK="off",
            GOTOOLCHAIN="local", GOFLAGS="-mod=readonly",
            GIT_CONFIG_NOSYSTEM="1", GIT_CONFIG_GLOBAL="/dev/null",
            GOCACHE=str(self.root / "go-cache"),
            GOLANGCI_LINT_CACHE=str(self.root / "lint-cache"),
            LINT_CACHE_ROOT="off", PYTHONDONTWRITEBYTECODE="1",
            LINT_TEST_LOG=str(self.log),
        )
        # Reuse compiled standard-library objects between independent fixtures.
        self.env["GOCACHE"] = str(Path(self.cache.name) / "go-cache")
        self.lint = self.root / "lint"
        self.lint.write_text("""#!/usr/bin/env python3
import json, os, subprocess, sys
with open(os.environ["LINT_TEST_LOG"], "a") as handle:
    handle.write(json.dumps(sys.argv[1:]) + "\\n")
real = os.environ.get("LINT_TEST_REAL")
args = [real, *sys.argv[1:]] if real else ["go", "vet", *sys.argv[2:]]
sys.exit(subprocess.call(args))
""")
        self.lint.chmod(0o755)
        if os.environ.get("LINT_SCOPE_TEST_LINTER"):
            self.env["LINT_TEST_REAL"] = os.environ["LINT_SCOPE_TEST_LINTER"]
        self.write("go.mod", "module example.org/scope\n\ngo 1.23\n")
        self.write(".golangci.yml", 'version: "2"\nrun:\n  allow-parallel-runners: true\nlinters:\n  default: none\n  enable: [govet]\n')
        self.write("alpha/alpha.go", "package alpha\nfunc Value() int { return 1 }\n")
        self.write("beta/beta.go", 'package beta\nimport "example.org/scope/alpha"\nfunc Value() int { return alpha.Value() }\n')
        self.write("gamma/gamma.go", 'package gamma\nimport "example.org/scope/beta"\nfunc Value() int { return beta.Value() }\n')
        self.write("external/external.go", "package external\n")
        self.write("external/external_test.go", 'package external_test\nimport ("testing"; "example.org/scope/gamma")\nfunc TestValue(t *testing.T) { _ = gamma.Value() }\n')
        self.write("other/other.go", "package other\nfunc Value() int { return 1 }\n")
        self.git("init", "-q", "-b", "main")
        self.git("config", "user.name", "scope-test")
        self.git("config", "user.email", "scope@example.invalid")
        self.commit()
        self.git("branch", "base")

    def write(self, name, text):
        path = self.repo / name
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(text)

    def git(self, *args):
        return subprocess.check_output(["git", *args], cwd=self.repo, env=self.env, stderr=subprocess.STDOUT)

    def commit(self):
        self.git("add", ".")
        self.git("commit", "-qm", "fixture")

    def finding(self, package):
        self.write(f"{package}/finding.go",
                   f'package {package}\nimport "fmt"\nfunc Finding() {{ fmt.Printf("%d", "wrong") }}\n')

    def run_lint(self, base="base", flags=()):
        env = dict(self.env)
        if base is not None:
            env["LINT_AFFECTED_BASE"] = base
        result = subprocess.run(
            ["python3", str(ROOT / "scripts/affected-preflight"), "lint", str(self.lint), *flags],
            cwd=self.repo, env=env, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True,
        )
        calls = [json.loads(line) for line in self.log.read_text().splitlines()] if self.log.exists() else []
        return result, calls

    def assert_scope(self, result, calls, packages, mode="affected"):
        self.assertIn(f"lint-scope: mode={mode} packages={len(packages) if mode != 'full' else 5} reason=", result.stdout)
        self.assertEqual(calls, [["run", *packages]], result.stdout)

    def test_changed_package_finding_fails(self):
        self.finding("alpha")
        self.commit()
        result, calls = self.run_lint()
        self.assertNotEqual(result.returncode, 0, result.stdout)
        self.assertIn("fmt.Printf", result.stdout)
        self.assert_scope(result, calls, ["./alpha", "./beta", "./external", "./gamma"])

    def test_transitive_importer_finding_fails(self):
        self.finding("gamma")
        self.commit()
        self.git("branch", "-f", "base")
        self.write("alpha/alpha.go", "package alpha\nfunc Value() int { return 2 }\n")
        self.commit()
        result, calls = self.run_lint()
        self.assertNotEqual(result.returncode, 0, result.stdout)
        self.assertIn("fmt.Printf", result.stdout)
        self.assert_scope(result, calls, ["./alpha", "./beta", "./external", "./gamma"])

    def test_unrelated_finding_is_outside_scope_and_default_still_fails(self):
        self.finding("other")
        self.commit()
        self.git("branch", "-f", "base")
        self.write("alpha/alpha.go", "package alpha\nfunc Value() int { return 2 }\n")
        self.commit()
        result, calls = self.run_lint()
        self.assertEqual(result.returncode, 0, result.stdout)
        self.assert_scope(result, calls, ["./alpha", "./beta", "./external", "./gamma"])
        self.log.unlink()
        result, calls = self.run_lint(base=None)
        self.assertNotEqual(result.returncode, 0, result.stdout)
        self.assert_scope(result, calls, ["./..."], "full")

    def test_each_tooling_trigger_runs_full(self):
        for path in (".golangci.yml", ".golangci.yaml", "go.mod", "go.sum",
                     "Makefile", "lint.mk", "go.work", "tools/lint/version",
                     "scripts/lint-with-cache", "scripts/affected-preflight"):
            with self.subTest(path=path):
                self.git("reset", "--hard", "base")
                self.git("clean", "-fdq")
                # These remain valid Go inputs so the actual full lint can run.
                text = {"go.mod": "module example.org/scope\n\ngo 1.23\n// changed\n",
                        "go.work": "go 1.23\nuse .\n"}.get(path, "# changed\n")
                if path in (".golangci.yml", ".golangci.yaml"):
                    text = (self.repo / ".golangci.yml").read_text() + text
                self.write(path, text)
                self.commit()
                self.log.unlink(missing_ok=True)
                result, calls = self.run_lint()
                self.assertEqual(result.returncode, 0, result.stdout)
                self.assert_scope(result, calls, ["./..."], "full")
                self.assertIn(path, result.stdout)

    def test_selection_failure_and_unreachable_base_run_full(self):
        self.write("alpha/alpha.go", "package alpha\nfunc Value() int { return 2 }\n")
        self.commit()
        for base, go in (("missing-base", None), ("base", "/no/such/go")):
            with self.subTest(base=base, go=go):
                if go:
                    self.env["GO"] = go
                self.log.unlink(missing_ok=True)
                result, calls = self.run_lint(base)
                self.assertEqual(result.returncode, 0, result.stdout)
                self.assertEqual(calls, [["run", "./..."]], result.stdout)
                self.assertIn("mode=full", result.stdout)
                self.assertIn("reason=", result.stdout)

    def test_empty_committed_go_change_set_explicitly_skips(self):
        self.write("README.md", "text\n")
        self.commit()
        result, calls = self.run_lint()
        self.assertEqual(result.returncode, 0, result.stdout)
        self.assertIn("mode=skip packages=0 reason=no affected Go packages", result.stdout)
        self.assertEqual(calls, [])

    def test_deleted_package_runs_full(self):
        shutil.rmtree(self.repo / "other")
        self.commit()
        result, calls = self.run_lint()
        self.assertEqual(result.returncode, 0, result.stdout)
        self.assertEqual(calls, [["run", "./..."]], result.stdout)
        self.assertIn("mode=full packages=4", result.stdout)
        self.assertIn("other/other.go", result.stdout)

    def test_unreadable_diff_runs_full(self):
        self.write("alpha/alpha.go", "package alpha\nfunc Value() int { return 2 }\n")
        self.commit()
        bindir = self.root / "bin"
        bindir.mkdir()
        git = bindir / "git"
        real_git = shutil.which("git")
        git.write_text(f'#!/bin/sh\nif [ "$1" = diff ]; then echo unreadable-diff >&2; exit 128; fi\nexec "{real_git}" "$@"\n')
        git.chmod(0o755)
        self.env["PATH"] = str(bindir) + os.pathsep + self.env["PATH"]
        result, calls = self.run_lint()
        self.assertEqual(result.returncode, 0, result.stdout)
        self.assert_scope(result, calls, ["./..."], "full")
        self.assertIn("unreadable-diff", result.stdout)

    def test_uncommitted_go_changes_run_full(self):
        self.finding("other")
        result, calls = self.run_lint()
        self.assertNotEqual(result.returncode, 0, result.stdout)
        self.assert_scope(result, calls, ["./..."], "full")
        self.assertIn("checkout differs", result.stdout)

    def test_malformed_package_graph_runs_full(self):
        bad_go = self.root / "bad-go"
        bad_go.write_text("#!/bin/sh\nprintf 'not-json'\n")
        bad_go.chmod(0o755)
        self.env["GO"] = str(bad_go)
        result, calls = self.run_lint()
        self.assertEqual(result.returncode, 0, result.stdout)
        self.assertEqual(calls, [["run", "./..."]], result.stdout)
        self.assertIn("mode=full packages=unknown", result.stdout)
        self.assertIn("malformed JSON", result.stdout)

    def test_custom_flags_run_full_and_linter_errors_propagate(self):
        result, calls = self.run_lint(flags=["--invalid-lint-flag"])
        self.assertNotEqual(result.returncode, 0, result.stdout)
        self.assertEqual(calls, [["run", "--invalid-lint-flag", "./..."]], result.stdout)
        self.assertIn("mode=full packages=5", result.stdout)
        self.assertIn("custom LINT_FLAGS", result.stdout)

    def test_merge_base_excludes_changes_only_on_the_base_branch(self):
        self.git("checkout", "-q", "base")
        self.write("Makefile", "# changed only on base\n")
        self.commit()
        self.git("checkout", "-q", "main")
        self.write("alpha/alpha.go", "package alpha\nfunc Value() int { return 2 }\n")
        self.commit()
        result, calls = self.run_lint()
        self.assertEqual(result.returncode, 0, result.stdout)
        self.assert_scope(result, calls, ["./alpha", "./beta", "./external", "./gamma"])

    def test_make_target_forwards_the_opt_in_and_preserves_full_default(self):
        self.finding("other")
        self.commit()
        self.git("branch", "-f", "base")
        self.write("alpha/alpha.go", "package alpha\nfunc Value() int { return 2 }\n")
        self.commit()
        for base, mode, expected in (
                ("base", "affected", ["./alpha", "./beta", "./external", "./gamma"]),
                ("", "full", ["./..."])):
            with self.subTest(base=base):
                self.log.unlink(missing_ok=True)
                result = subprocess.run(
                    ["make", "--no-print-directory", "-f", str(ROOT / "Makefile"),
                     f"GOLANGCI_LINT={self.lint}", f"LINT_AFFECTED_BASE={base}",
                     "SYS_USR_CGO_FALLBACK=0", "lint-golangci"],
                    cwd=self.repo, env=self.env,
                    stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True,
                )
                calls = [json.loads(line) for line in self.log.read_text().splitlines()]
                self.assert_scope(result, calls, expected, mode)
                self.assertEqual(result.returncode == 0, mode == "affected", result.stdout)

    def test_embedded_asset_selects_its_package_and_importers(self):
        self.write("alpha/asset.txt", "before")
        self.write("alpha/embed.go", 'package alpha\nimport _ "embed"\n//go:embed asset.txt\nvar Asset string\n')
        self.commit()
        self.git("branch", "-f", "base")
        self.write("alpha/asset.txt", "after")
        self.commit()
        result, calls = self.run_lint()
        self.assertEqual(result.returncode, 0, result.stdout)
        self.assert_scope(result, calls, ["./alpha", "./beta", "./external", "./gamma"])


if __name__ == "__main__":
    unittest.main()
