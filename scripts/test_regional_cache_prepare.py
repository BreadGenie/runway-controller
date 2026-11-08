"""Fresh-clone cache preparation and rollback, without privileged system changes."""
import base64
import importlib.util
import json
import io
import sys
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
from types import SimpleNamespace
from unittest.mock import patch

spec = importlib.util.spec_from_file_location('regional_guest', Path(__file__).resolve().parents[1] / 'guest/guest_agent.py')
guest = importlib.util.module_from_spec(spec)
spec.loader.exec_module(guest)


class PrepareTests(unittest.TestCase):
    def fixture(self, root):
        runner = root / 'runner'
        (runner / 'bin').mkdir(parents=True)
        (runner / 'bin/Runner.Listener').write_text('listener')
        for version in ('node20', 'node24'):
            node = runner / 'externals' / version / 'bin/node'
            node.parent.mkdir(parents=True)
            node.write_bytes(version.encode())
            node.chmod(0o755)
        paths = guest._Paths(system_root=root, runner=runner, marker=root / 'marker')
        files = {'ca': Path('/etc/ssl/certs/ca-certificates.crt').read_bytes(),
                 'node': b'\x7fELFnew wrapper',
                 'allowlist': (Path(__file__).resolve().parents[1] / 'cmd/cache-node/allowlist.json').read_bytes()}
        bundle = {key: base64.b64encode(value).decode() for key, value in files.items()}
        for helper in ('opt/runway/bin/cache-relay', 'etc/systemd/system/runway-cache-relay.service'):
            target = root / helper
            target.parent.mkdir(parents=True, exist_ok=True)
            target.write_text('baked helper')
            target.chmod(0o755 if helper.endswith('cache-relay') else 0o644)
        for directory in root.rglob('*'):
            if directory.is_dir():
                directory.chmod(0o755)
        bundle['service'] = 'internal-cache:443'
        return paths, bundle

    def run_guest(self, paths, bundle, *, action='prepare', fail=None, existing_service=False):
        commands = []
        real_lstat = os.lstat
        def lstat(path, *args, **kwargs):
            value = list(real_lstat(path, *args, **kwargs))
            value[4] = value[5] = 0  # guest files are root-owned; tests run unprivileged
            return os.stat_result(value)
        def run(argv, **kwargs):
            commands.append(argv)
            # No pre-existing relay, then active after restart.
            inactive = not existing_service and (argv[:2] == ['systemctl', 'is-enabled'] or (
                argv[:2] == ['systemctl', 'is-active'] and not any(c[:2] == ['systemctl', 'restart'] for c in commands)))
            failed = fail and argv[:len(fail)] == fail
            return subprocess.CompletedProcess(argv, int(bool(inactive or failed)),
                                               'cache-node/v1 allowlist/v1\n' if '--runway-cache-check-allowlist' in argv else '')
        with patch.object(guest.socket, 'create_connection'), patch.object(guest.subprocess, 'run', side_effect=run), patch.object(guest.pwd, 'getpwnam', return_value=SimpleNamespace(pw_uid=0, pw_gid=0)), patch.object(guest.os, 'fchown'), patch.object(guest.os, 'lstat', side_effect=lstat):
            result = guest.execute({'action': action, 'runner_name': 'runway-test', **({'regional_cache': bundle} if bundle else {})}, _paths=paths)
        return result, commands

    def test_opt_in_and_idempotent_preserves_real_nodes(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            paths, bundle = self.fixture(root)
            self.assertEqual(self.run_guest(paths, None)[0], (0, {'state': 'installed'}))
            self.assertFalse((root / 'etc/runway/cache.json').exists())
            for _ in range(2):
                self.assertEqual(self.run_guest(paths, bundle)[0], (0, {'state': 'installed'}))
                for version in ('node20', 'node24'):
                    node = paths.runner / 'externals' / version / 'bin/node'
                    self.assertEqual(node.read_bytes(), b'\x7fELFnew wrapper')
                    self.assertEqual(node.with_name('node.real').read_bytes(), version.encode())
            self.assertEqual(json.loads((root / 'etc/runway/cache.json').read_text()),
                             {'version': 1, 'mode': 'service', 'service': 'internal-cache:443', 'ca_file': '/etc/runway/cache-ca.pem'})

    def test_baked_default_and_missing_or_old_wrapper_restore(self):
        for broken in (None, 'missing', 'missing-real', 'old', 'schema'):
            with self.subTest(broken=broken), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                paths, bundle = self.fixture(root)
                self.assertEqual(self.run_guest(paths, bundle)[0][0], 0)
                del bundle['node']
                if broken == 'missing':
                    (paths.runner / 'externals/node24/bin/node').unlink()
                if broken == 'missing-real':
                    (paths.runner / 'externals/node24/bin/node.real').unlink()
                if broken == 'schema':
                    bundle['allowlist'] = base64.b64encode(b'{"schema":"future"}').decode()
                before = {p.relative_to(root): p.read_bytes() for p in root.rglob('*') if p.is_file()}
                failure = [str(paths.runner / 'externals/node24/bin/node')] if broken in ('old', 'schema') else None
                result, commands = self.run_guest(paths, bundle, fail=failure)
                self.assertEqual(result[0], int(broken is not None))
                self.assertEqual(before, {p.relative_to(root): p.read_bytes() for p in root.rglob('*') if p.is_file()})
                self.assertFalse(any('runway.jit' in str(c) for c in commands))
                if broken:
                    self.assertFalse(any(c[:2] == ['systemctl', 'restart'] for c in commands))

    def test_failure_restores_fresh_and_previous_install(self):
        for previous in (False, True):
            for failure in (['systemd-tmpfiles'], ['systemctl', 'daemon-reload'], ['systemctl', 'enable'], ['systemctl', 'restart'], ['systemctl', 'is-active']):
                with self.subTest(previous=previous, failure=failure), tempfile.TemporaryDirectory() as directory:
                    root = Path(directory)
                    paths, bundle = self.fixture(root)
                    if previous:
                        self.assertEqual(self.run_guest(paths, bundle)[0][0], 0)
                        bundle['node'] = base64.b64encode(b'\x7fELFreplacement').decode()
                    before = {p.relative_to(root): (p.read_bytes(), p.stat().st_mode) for p in root.rglob('*') if p.is_file()}
                    self.assertEqual(self.run_guest(paths, bundle, fail=failure)[0], (1, {'state': 'unknown'}))
                    after = {p.relative_to(root): (p.read_bytes(), p.stat().st_mode) for p in root.rglob('*') if p.is_file()}
                    self.assertEqual(before, after)

    def test_failure_restores_active_enabled_service(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            paths, bundle = self.fixture(root)
            self.assertEqual(self.run_guest(paths, bundle)[0][0], 0)
            result, commands = self.run_guest(paths, bundle, fail=['systemctl', 'restart'], existing_service=True)
            self.assertEqual(result[0], 1)
            unit = 'runway-cache-relay.service'
            self.assertIn(['systemctl', 'stop', unit], commands)
            self.assertIn(['systemctl', 'start', unit], commands)
            self.assertNotIn(['systemctl', 'disable', unit], commands)

    def test_registered_and_status_never_install(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            paths, bundle = self.fixture(root)
            self.assertEqual(self.run_guest(paths, bundle, action='status')[0][0], 0)
            (paths.runner / '.runner').write_text('registered')
            self.assertEqual(self.run_guest(paths, bundle)[0][0], 1)
            self.assertFalse((root / 'etc/runway/cache.json').exists())

    def test_invalid_ca_or_symlink_restores_nodes(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            paths, bundle = self.fixture(root)
            (root / 'etc/runway').symlink_to(root / 'runner', target_is_directory=True)
            self.assertEqual(self.run_guest(paths, bundle)[0][0], 1)
            for version in ('node20', 'node24'):
                self.assertEqual((paths.runner / 'externals' / version / 'bin/node').read_bytes(), version.encode())
            bundle['ca'] = base64.b64encode(b'not a CA').decode()
            self.assertEqual(self.run_guest(paths, bundle)[0][0], 1)


class TransportTests(unittest.TestCase):
    def test_files_travel_in_prepare_stdin_only(self):
        spec = importlib.util.spec_from_file_location('regional_transport', Path(__file__).with_name('metal-guest-transport.py'))
        transport = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(transport)
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / 'node').write_bytes(b'ELF binary')
            (root / 'ca').write_bytes(b'CA PEM')
            for override, action in ((override, action) for override in (False, True) for action in ('prepare', 'status', 'bootstrap', 'stop')):
                argv = ['transport', '--host', 'metal', '--key-file', '/key', '--known-hosts', '/hosts',
                        '--vm-id', 'vm-1', '--runner-name', 'runway-test', '--regional-cache', 'internal-cache:443',
                        '--regional-cache-ca', str(root / 'ca')]
                if override:
                    argv += ['--regional-cache-node', str(root / 'node')]
                argv += [action]
                with patch.object(sys, 'argv', argv), patch.object(sys, 'stdin', io.StringIO('JIT')), patch.object(sys, 'stdout', io.StringIO()), patch.object(transport.subprocess, 'run', return_value=subprocess.CompletedProcess([], 0, '{"state":"installed"}')) as run:
                    self.assertEqual(transport.main(), 0)
                sent = json.loads(run.call_args.kwargs['input'])
                self.assertEqual('regional_cache' in sent, action == 'prepare')
                if action == 'prepare':
                    if override:
                        self.assertEqual(base64.b64decode(sent['regional_cache']['node']), b'ELF binary')
                    else:
                        self.assertNotIn('node', sent['regional_cache'])
                        size_payload = json.loads(json.dumps(sent))
                        size_payload['regional_cache'].pop('ca')
                        self.assertLess(len(json.dumps(size_payload).encode()), 64 * 1024)
                    self.assertEqual(base64.b64decode(sent['regional_cache']['ca']), b'CA PEM')
                    self.assertNotIn('relay', sent['regional_cache'])
                    self.assertNotIn('unit', sent['regional_cache'])
                    self.assertEqual(json.loads(base64.b64decode(sent['regional_cache']['allowlist'])),
                                     json.loads((Path(__file__).resolve().parents[1] / 'cmd/cache-node/allowlist.json').read_text()))
