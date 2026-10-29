#!/usr/bin/env python3
"""Fast filesystem proofs for lint seed isolation and invalidation."""

import importlib.machinery
import importlib.util
from concurrent.futures import ThreadPoolExecutor
from pathlib import Path
import shutil
import tempfile
import threading
import unittest
from unittest.mock import patch


SCRIPT = Path(__file__).with_name("lint-with-cache")


class LintCacheTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        loader = importlib.machinery.SourceFileLoader("lint_cache", str(SCRIPT))
        spec = importlib.util.spec_from_loader(loader.name, loader)
        cls.cache = importlib.util.module_from_spec(spec)
        loader.exec_module(cls.cache)

    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)

    def test_seed_is_private_and_preserves_red_and_green_verdicts(self):
        store = self.root / "store"
        calls = []

        def lint(command, env):
            go = Path(env["GOCACHE"])
            lint = Path(env["GOLANGCI_LINT_CACHE"])
            calls.append((go, lint))
            self.assertEqual(go.parent, lint.parent)
            self.assertNotEqual(go.parent.parent, store / "seeds")
            if len(calls) == 1:
                (go / "compiled").write_text("build")
                (lint / "issues").write_text("red")
                return 1
            self.assertEqual((go / "compiled").read_text(), "build")
            self.assertEqual((lint / "issues").read_text(), "red")
            seed = store / "seeds" / "ab-key" / "lint" / "issues"
            self.assertNotEqual(seed.stat().st_ino, (lint / "issues").stat().st_ino)
            (lint / "issues").write_text("private change")
            return 0

        for verdict in (1, 0):
            self.assertEqual(self.cache.run_cached(store, "ab-key", True, [], {}, lint, 1024), verdict)
        self.assertEqual((store / "seeds" / "ab-key" / "lint" / "issues").read_text(), "red")
        self.assertNotEqual(calls[0], calls[1])
        self.assertEqual(list((store / "runs").iterdir()), [])

    def test_legacy_linter_shares_go_entries_but_never_result_paths(self):
        store = self.root / "store"
        runs = []

        def lint(command, env):
            go = Path(env["GOCACHE"])
            result = Path(env["GOLANGCI_LINT_CACHE"])
            self.assertEqual(list(result.iterdir()), [])
            if runs:
                self.assertEqual((go / "compiled").read_text(), "build")
            (go / "compiled").write_text("build")
            (result / "stale-path").write_text("/deleted/checkout.go")
            runs.append(True)
            return 0

        for _ in range(2):
            self.cache.run_cached(store, "ab-key", False, [], {}, lint, 1024)
        self.assertFalse((store / "seeds" / "ab-key" / "lint").exists())

    def test_warm_gates_lint_concurrently_without_sharing_writes(self):
        store = self.root / "store"

        def seed(command, env):
            (Path(env["GOCACHE"]) / "entry").write_text("seed")
            return 0

        self.cache.run_cached(store, "ab-key", True, [], {}, seed, 1024)
        barrier = threading.Barrier(2, timeout=5)
        private_paths = []

        def lint(command, env):
            private = Path(env["GOCACHE"])
            private_paths.append(private)
            self.assertEqual((private / "entry").read_text(), "seed")
            (private / "entry").write_text(str(private))
            barrier.wait()
            self.assertEqual((private / "entry").read_text(), str(private))
            return 0

        with ThreadPoolExecutor(max_workers=2) as workers:
            futures = [workers.submit(self.cache.run_cached, store, "ab-key", True, [], {}, lint, 1024) for _ in range(2)]
            self.assertEqual([job.result(timeout=10) for job in futures], [0, 0])
        self.assertNotEqual(*private_paths)
        self.assertEqual((store / "seeds" / "ab-key" / "go" / "entry").read_text(), "seed")

    def test_concurrent_first_gates_publish_one_seed(self):
        store = self.root / "store"
        barrier = threading.Barrier(2, timeout=5)
        cold_runs = []

        def lint(command, env):
            entry = Path(env["GOCACHE"]) / "entry"
            if not entry.exists():
                cold_runs.append(True)
            entry.write_text("seed")
            return 0

        def gate():
            barrier.wait()
            return self.cache.run_cached(store, "ab-key", True, [], {}, lint, 1024)

        with ThreadPoolExecutor(max_workers=2) as workers:
            futures = [workers.submit(gate) for _ in range(2)]
            self.assertEqual([job.result(timeout=10) for job in futures], [0, 0])
        self.assertEqual(len(cold_runs), 1)

    def test_failed_or_interrupted_lint_does_not_publish_or_leak_private_data(self):
        store = self.root / "store"

        def lint(command, env):
            (Path(env["GOCACHE"]) / "partial").write_text("partial")
            return 3

        self.assertEqual(self.cache.run_cached(store, "ab-key", True, [], {}, lint, 1024), 3)
        self.assertFalse((store / "seeds" / "ab-key").exists())
        self.assertEqual(list((store / "runs").iterdir()), [])

        def interrupted(command, env):
            raise InterruptedError("terminated")

        with self.assertRaises(InterruptedError):
            self.cache.run_cached(store, "ab-key", True, [], {}, interrupted, 1024)
        self.assertEqual(list((store / "runs").iterdir()), [])

    def test_retained_seeds_obey_byte_budget(self):
        store = self.root / "store"

        def lint(command, env):
            (Path(env["GOCACHE"]) / "entry").write_bytes(b"x" * 80)
            return 0

        for key in ("ab-first", "cd-second"):
            self.cache.run_cached(store, key, True, [], {}, lint, 100)
        self.assertFalse((store / "seeds" / "ab-first").exists())
        self.assertTrue((store / "seeds" / "cd-second").exists())
        self.cache.run_cached(store, "ef-too-big", True, [], {}, lint, 50)
        self.assertFalse((store / "seeds" / "ef-too-big").exists())
        self.assertLessEqual(sum(self.cache.tree_size(path) for path in (store / "seeds").iterdir()), 50)

    def test_key_changes_with_every_declared_input_but_not_checkout_path(self):
        lint = self.root / "golangci-lint"
        lint.write_text("binary")
        checkout = self.root / "checkout"
        checkout.mkdir()
        for name in ("go.mod", "go.sum", ".golangci.yml"):
            (checkout / name).write_text(name)
        version = ["golangci-lint has version 2.14.0 built with go1.27.3"]
        goenv = [{"GOVERSION": "go1.26.9", "GOOS": "linux", "GOCACHE": "private"}]

        def output(command, **kwargs):
            import json
            if command[0] == "go":
                return json.dumps(goenv[0])
            if command[1] == "version":
                return version[0]
            return str(checkout / ".golangci.yml")

        with patch.object(self.cache.subprocess, "check_output", side_effect=output):
            key, share = self.cache.cache_identity(str(lint), ["run", "./..."], {}, checkout)
            self.assertTrue(share)
            for name in ("go.mod", "go.sum", ".golangci.yml"):
                file = checkout / name
                original = file.read_text()
                file.write_text(original + " changed")
                self.assertNotEqual(key, self.cache.cache_identity(str(lint), ["run", "./..."], {}, checkout)[0])
                file.write_text(original)
            for modified in ("binary bump", "different custom build"):
                lint.write_text(modified)
                self.assertNotEqual(key, self.cache.cache_identity(str(lint), ["run", "./..."], {}, checkout)[0])
            lint.write_text("binary")
            goenv[0]["GOCACHE"] = "another private path"
            self.assertEqual(key, self.cache.cache_identity(str(lint), ["run", "./..."], {}, checkout)[0])
            # Go synthesizes this flag with a fresh go-build directory on
            # every env query. Compiler inputs are represented separately.
            goenv[0]["GOGCCFLAGS"] = "-ffile-prefix-map=/scratch/go-build123=/tmp/go-build"
            self.assertEqual(key, self.cache.cache_identity(str(lint), ["run", "./..."], {}, checkout)[0])
            peer = self.root / "peer"
            shutil.copytree(checkout, peer)
            shutil.rmtree(checkout)
            checkout = peer
            self.assertEqual(key, self.cache.cache_identity(str(lint), ["run", "./..."], {}, checkout)[0])
            self.assertNotEqual(key, self.cache.cache_identity(str(lint), ["run", "--build-tags=other", "./..."], {}, checkout)[0])
            goenv[0]["GOVERSION"] = "go1.27.0"
            self.assertNotEqual(key, self.cache.cache_identity(str(lint), ["run", "./..."], {}, checkout)[0])
            goenv[0]["GOVERSION"] = "go1.26.9"
            version[0] = "golangci-lint has version 2.12.0 built with go1.26.9"
            other, share = self.cache.cache_identity(str(lint), ["run", "./..."], {}, checkout)
            self.assertFalse(share)
            self.assertNotEqual(key, other)


if __name__ == "__main__":
    unittest.main()
