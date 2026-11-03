#!/usr/bin/env python3
"""Exercise the unit sweep's real fan-out with an isolated fake Go tool."""
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import tempfile
import unittest


class UnitPackageRunnerTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        scripts = self.root / 'scripts'
        scripts.mkdir()
        source = Path(__file__).resolve().parent
        for name in ('test-local-parallel', 'test-go-test-shard', 'go-test-observable',
                     'test-gitconfig-path', 'push-gate-lock-lib.sh', 'lib'):
            target = source / name
            if target.is_dir():
                shutil.copytree(target, scripts / name)
            else:
                shutil.copy2(target, scripts / name)
        self.bin = self.root / 'bin'
        self.bin.mkdir()
        self.calls = self.root / 'calls'
        self.calls.mkdir()
        fake = self.bin / 'go'
        fake.write_text('''#!/usr/bin/env python3
import json, os, pathlib, sys
root = pathlib.Path(__file__).resolve().parent.parent
args = sys.argv[1:]
if args[0] == 'env':
    print(root)
elif args[0] == 'list':
    if (root / 'list-fail').exists():
        print('broken package enumeration', file=sys.stderr)
        sys.exit(19)
    if './cmd/gc' not in args:
        print('github.com/gastownhall/gascity/internal/config')
    if './internal/config' not in args:
        print('github.com/gastownhall/gascity/cmd/gc')
elif args[0] == 'test':
    if '-list' in args:
        inventory = root / 'inventory.json'
        names = json.loads(inventory.read_text()) if inventory.exists() else [
            'TestAlpha', 'TestBeta', 'TestGamma', 'TestDelta', 'ExampleUsage', 'FuzzInput']
        print('\\n'.join(names))
    else:
        (root / 'calls' / str(os.getpid())).write_text(json.dumps(dict(args=args, env=dict(os.environ))))
        print(json.dumps(dict(Action='pass', Package='fixture', Elapsed=0.01)))
        if (root / 'test-fail').exists() and '-run' in args:
            sys.exit(23)
else:
    sys.exit(99)
''')
        fake.chmod(0o755)
        self.env = dict(os.environ, PATH=f'{self.bin}:{os.environ["PATH"]}',
                        HOME=str(self.root), TMPDIR=str(self.root),
                        GC_TEST_NO_SLICE='1', GC_TEST_NO_ORPHAN_SWEEP='1', GC_PUSH_GATE_NO_CAP='1',
                        LOCAL_TEST_JOBS='2', CMD_GC_PROCESS_TOTAL='2',
                        GO_TEST_TIMEOUT='37m', SYS_USR_CGO_FALLBACK='0',
                        ANTHROPIC_API_KEY='fixture-only', ZCODE_MODEL='fixture-model',
                        GC_AGENT='must-not-reach-tests', GC_DOLT_PORT='1',
                        BEADS_DOLT_SERVER_PORT='2',
                        OBSERVABLE_TEST_LOG=str(self.root / 'tests.jsonl'))

    def run_sweep(self, *packages):
        return subprocess.run([str(self.root / 'scripts/test-local-parallel'), 'unit', *packages],
                              env=self.env, text=True, capture_output=True, timeout=60)

    def records(self):
        return [json.loads(p.read_text()) for p in self.calls.iterdir()]

    def test_full_sweep_covers_every_test_once_with_bounded_jobs(self):
        run = self.run_sweep('./...')
        self.assertEqual(run.returncode, 0, run.stdout + run.stderr)
        self.assertIn('LOCAL_TEST_JOBS=2 inner_p=1', run.stdout)
        records = self.records()
        self.assertEqual(len(records), 3)
        core = [r for r in records if '-run' not in r['args']]
        self.assertEqual(len(core), 1)
        self.assertIn('github.com/gastownhall/gascity/internal/config', core[0]['args'])
        self.assertNotIn('github.com/gastownhall/gascity/cmd/gc', core[0]['args'])
        selected = []
        for r in records:
            self.assertIn('-count=1', r['args'])
            self.assertEqual(r['args'][r['args'].index('-timeout') + 1], '37m')
            self.assertEqual(r['env']['GC_FAST_UNIT'], '1')
            self.assertTrue(r['env']['GOFLAGS'].endswith('-p=1'))
            self.assertEqual(r['env']['SHELL'], '/bin/sh')
            self.assertEqual(r['env']['ANTHROPIC_API_KEY'], 'fixture-only')
            self.assertEqual(r['env']['ZCODE_MODEL'], 'fixture-model')
            self.assertEqual(r['env']['GIT_CONFIG_NOSYSTEM'], '1')
            for key in ('GC_AGENT', 'GC_DOLT_PORT', 'BEADS_DOLT_SERVER_PORT'):
                self.assertNotIn(key, r['env'])
            if '-run' in r['args']:
                selected += r['args'][r['args'].index('-run') + 1][2:-2].split('|')
        self.assertCountEqual(selected, ['TestAlpha', 'TestBeta', 'TestGamma', 'TestDelta',
                                         'ExampleUsage', 'FuzzInput'])
        self.assertEqual(len(list(self.root.glob('tests-*.jsonl'))), 3)

    def test_scoped_sweep_does_not_add_cmd_gc(self):
        run = self.run_sweep('./internal/config')
        self.assertEqual(run.returncode, 0, run.stdout + run.stderr)
        self.assertEqual(len(self.records()), 1)
        self.assertNotIn('-run', self.records()[0]['args'])

    def test_large_inventory_executes_exact_shards_without_oversized_arguments(self):
        # Each uncompressed shard exceeds Linux's 128 KiB single-argument cap.
        names = [f'TestControllerReconciliationPreservesConfiguredSessionIdentity{i:05d}'
                 for i in range(6000)]
        # A test name can be the prefix of another; neither may be lost or widened.
        names += [names[0] + 'Extra', 'ExampleUsage', 'FuzzInput']
        (self.root / 'inventory.json').write_text(json.dumps(names))
        run = self.run_sweep('./cmd/gc')
        self.assertEqual(run.returncode, 0, run.stdout + run.stderr)
        records = self.records()
        self.assertEqual(len(records), 2)
        selected = []
        for record in records:
            pattern = record['args'][record['args'].index('-run') + 1]
            self.assertLess(len(pattern.encode()), 128 * 1024)
            matches = [name for name in names if re.fullmatch(pattern, name)]
            shard = int(record['env']['GC_TEST_SHARD_INDEX'])
            self.assertEqual(matches, names[shard - 1::2])
            self.assertIsNone(re.fullmatch(pattern, 'TestUnselected'))
            self.assertIsNone(re.fullmatch(pattern, names[0] + 'UnselectedSuffix'))
            selected += matches
        self.assertCountEqual(selected, names)

    def test_cmd_gc_only_has_no_core_sweep(self):
        # go list supplies canonical import paths, including for relative selectors.
        run = self.run_sweep('./cmd/gc')
        self.assertEqual(run.returncode, 0, run.stdout + run.stderr)
        self.assertEqual(len(self.records()), 2)
        self.assertTrue(all('-run' in r['args'] for r in self.records()))

    def test_unicode_inventory_measures_argument_bytes(self):
        # Go identifiers allow these four-byte letters. Character counts alone
        # leave each shard below the compaction threshold but above exec's cap.
        names = ['Test' + '\U00020000' * 80 + str(i) for i in range(900)]
        (self.root / 'inventory.json').write_text(json.dumps(names))
        run = self.run_sweep('./cmd/gc')
        self.assertEqual(run.returncode, 0, run.stdout + run.stderr)
        records = self.records()
        self.assertEqual(len(records), 2)
        for record in records:
            pattern = record['args'][record['args'].index('-run') + 1]
            self.assertLess(len(pattern.encode()), 128 * 1024)
            shard = int(record['env']['GC_TEST_SHARD_INDEX'])
            self.assertEqual([name for name in names if re.fullmatch(pattern, name)],
                             names[shard - 1::2])

    def test_selection_that_cannot_fit_fails_before_running_tests(self):
        names = ['Test' + hashlib.sha256(str(i).encode()).hexdigest() * 3
                 for i in range(2000)]
        (self.root / 'inventory.json').write_text(json.dumps(names))
        run = self.run_sweep('./cmd/gc')
        self.assertNotEqual(run.returncode, 0, run.stdout + run.stderr)
        self.assertIn('increase the shard total', run.stderr)
        self.assertEqual(self.records(), [])

    def test_enumeration_failure_is_not_an_empty_pass(self):
        (self.root / 'list-fail').touch()
        run = self.run_sweep('./...')
        self.assertNotEqual(run.returncode, 0, run.stdout + run.stderr)
        self.assertIn('broken package enumeration', run.stderr)
        self.assertEqual(self.records(), [])

    def test_unit_sweep_without_provider_inputs(self):
        for name in list(self.env):
            if name.startswith(('ANTHROPIC_', 'CLAUDE_CODE_', 'ZCODE_')) or name in ('OLLAMA_API_KEY', 'XIAOMI_API_KEY'):
                del self.env[name]
        run = self.run_sweep('./internal/config')
        self.assertEqual(run.returncode, 0, run.stdout + run.stderr)
        self.assertEqual(len(self.records()), 1)

    def test_empty_enumeration_fails_closed(self):
        fake = self.bin / 'go'
        fake.write_text(fake.read_text().replace(
            "elif args[0] == 'list':", "elif args[0] == 'list':\n    sys.exit(0)"))
        run = self.run_sweep('./...')
        self.assertNotEqual(run.returncode, 0, run.stdout + run.stderr)
        self.assertIn('returned no packages', run.stderr)
        self.assertEqual(self.records(), [])

    def test_failed_shard_fails_the_sweep(self):
        (self.root / 'test-fail').touch()
        run = self.run_sweep('./...')
        self.assertNotEqual(run.returncode, 0, run.stdout + run.stderr)
        self.assertIn('failed with exit 23', run.stderr)


if __name__ == '__main__':
    unittest.main()
