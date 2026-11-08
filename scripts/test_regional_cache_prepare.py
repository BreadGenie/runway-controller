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
                 'node': b'\x7fELFnew wrapper', 'relay': b'new relay', 'unit': b'new unit'}
        bundle = {key: base64.b64encode(value).decode() for key, value in files.items()}
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
            return subprocess.CompletedProcess(argv, int(bool(inactive or failed)), '')
        with patch.object(guest.socket, 'create_connection'), patch.object(guest.subprocess, 'run', side_effect=run), patch.object(guest.pwd, 'getpwnam', return_value=SimpleNamespace(pw_uid=0, pw_gid=0)), patch.object(guest.os, 'fchown'), patch.object(guest.os, 'lstat', side_effect=lstat):
            result = guest.execute({'action': action, 'runner_name': 'runway-test', **({'regional_cache': bundle} if bundle else {})}, _paths=paths)
        return result, commands

    def test_opt_in_and_idempotent_preserves_real_nodes(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            paths, bundle = self.fixture(root)
            self.assertEqual(self.run_guest(paths, None)[0], (0, {'state': 'installed'}))
            self.assertFalse((root / 'etc').exists())
            for _ in range(2):
                self.assertEqual(self.run_guest(paths, bundle)[0], (0, {'state': 'installed'}))
                for version in ('node20', 'node24'):
                    node = paths.runner / 'externals' / version / 'bin/node'
                    self.assertEqual(node.read_bytes(), b'\x7fELFnew wrapper')
                    self.assertEqual(node.with_name('node.real').read_bytes(), version.encode())
            self.assertEqual(json.loads((root / 'etc/runway/cache.json').read_text()),
                             {'version': 1, 'mode': 'service', 'service': 'internal-cache:443', 'ca_file': '/etc/runway/cache-ca.pem'})

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
            self.assertFalse((root / 'etc').exists())

    def test_invalid_ca_or_symlink_restores_nodes(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            paths, bundle = self.fixture(root)
            (root / 'etc').symlink_to(root / 'runner', target_is_directory=True)
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
            for action in ('prepare', 'status', 'bootstrap', 'stop'):
                argv = ['transport', '--host', 'metal', '--key-file', '/key', '--known-hosts', '/hosts',
                        '--vm-id', 'vm-1', '--runner-name', 'runway-test', '--regional-cache', 'internal-cache:443',
                        '--regional-cache-ca', str(root / 'ca'), '--regional-cache-node', str(root / 'node'), action]
                with patch.object(sys, 'argv', argv), patch.object(sys, 'stdin', io.StringIO('JIT')), patch.object(sys, 'stdout', io.StringIO()), patch.object(transport.subprocess, 'run', return_value=subprocess.CompletedProcess([], 0, '{"state":"installed"}')) as run:
                    self.assertEqual(transport.main(), 0)
                sent = json.loads(run.call_args.kwargs['input'])
                self.assertEqual('regional_cache' in sent, action == 'prepare')
                if action == 'prepare':
                    self.assertEqual(base64.b64decode(sent['regional_cache']['node']), b'ELF binary')
                    self.assertEqual(base64.b64decode(sent['regional_cache']['ca']), b'CA PEM')
                    self.assertIn('RestrictAddressFamilies=AF_INET AF_INET6 AF_VSOCK', base64.b64decode(sent['regional_cache']['unit']).decode())
