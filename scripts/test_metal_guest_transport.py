"""Verify that the administrative transport keeps JIT material out of argv."""

import importlib.util
import io
import json
import os
import stat
import subprocess
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

spec = importlib.util.spec_from_file_location(
    'metal_guest_transport', Path(__file__).with_name('metal-guest-transport.py')
)
transport = importlib.util.module_from_spec(spec)
spec.loader.exec_module(transport)

RUNNING_REPORT = 'LoadState=loaded\nActiveState=active\nExecMainStartTimestampMonotonic=123\n'
COMPLETED_REPORT = 'LoadState=loaded\nActiveState=inactive\nExecMainStartTimestampMonotonic=123\n'
SERVICE_REPORT = ('systemctl', 'show', 'runway-runner.service', '--property=LoadState',
                  '--property=ActiveState', '--property=ExecMainStartTimestampMonotonic')
SUDOERS_RULE = 'runner ALL=(ALL) NOPASSWD: ALL\n'


class TransportTests(unittest.TestCase):
    def arguments(self, action='bootstrap'):
        return ['transport', '--host', 'root@metal.test', '--key-file', '/keys/admin',
                '--known-hosts', '/keys/known', action, '--vm-id', 'vm-123',
                '--runner-name', 'runway-lease-123']

    def guest_paths(self, directory):
        """Substitute private temporary paths for a fresh unregistered guest tree."""
        root = Path(directory)
        (root / 'actions-runner/bin').mkdir(parents=True, exist_ok=True)
        (root / 'actions-runner/bin/Runner.Listener').write_text('')
        (root / 'sudoers').mkdir(parents=True, exist_ok=True)
        program = transport.GUEST_PROGRAM
        for guest_path, private_path in (('/var/lib/runway/bootstrap.json', root / 'bootstrap.json'),
                                         ('/home/runner/actions-runner', root / 'actions-runner'),
                                         ('/etc/sudoers.d/runway-runner', root / 'sudoers/runway-runner'),
                                         ('/etc/sudoers.d', root / 'sudoers')):
            program = program.replace(guest_path, str(private_path))
        return program, root / 'sudoers/runway-runner'

    def execute_guest(self, program, sudoers, payload, *, present=('sudo', 'visudo'),
                      results=None, plant=None):
        """Run the embedded guest program locally: no SSH and no real guest binary."""
        real_lstat = os.lstat
        recorded, ownership = [], []
        results = results or {}

        def fake_run(argv, **kwargs):
            recorded.append(list(argv))
            return subprocess.CompletedProcess(list(argv), *results.get(tuple(argv), (0, '')))

        def fake_which(name, path=None, **kwargs):
            return '/usr/bin/' + name if name in present else None

        def fake_fchown(descriptor, uid, gid):
            # An unprivileged test user cannot hand a temporary file to root: record
            # the ownership the guest program demands instead of granting it.
            ownership.append((uid, gid))

        def fake_lstat(path):
            if os.fspath(path) != os.fspath(sudoers):
                return os.stat_result(list(real_lstat(os.fspath(path))))
            try:
                entry = list(real_lstat(os.fspath(path)))
            except FileNotFoundError:
                if plant is not None:
                    plant()
                raise
            entry[4:6] = [0, 0]
            return os.stat_result(entry)

        with (patch.object(transport.sys, 'stdin', io.StringIO(json.dumps(payload))),
              patch.object(transport.sys, 'stdout', io.StringIO()) as output,
              patch('shutil.which', side_effect=fake_which),
              patch.object(transport.os, 'fchown', side_effect=fake_fchown),
              patch.object(transport.os, 'lstat', side_effect=fake_lstat),
              patch.object(transport.subprocess, 'run', side_effect=fake_run)):
            with self.assertRaises(SystemExit) as stopped:
                exec(program, {})
        return stopped.exception.code, output.getvalue(), recorded, ownership

    def test_jit_uses_stdin_not_argv_or_output(self):
        secret = 'one-time-private-configuration'
        with (patch.object(transport.sys, 'argv', self.arguments()),
              patch.object(transport.sys, 'stdin', io.StringIO(secret)),
              patch.object(transport.sys, 'stdout', io.StringIO()) as output,
              patch.object(transport.subprocess, 'run') as run):
            run.return_value = subprocess.CompletedProcess([], 0, '{"state":"running"}')
            self.assertEqual(transport.main(), 0)
            argv = run.call_args.args[0]
            payload = json.loads(run.call_args.kwargs['input'])
            self.assertNotIn(secret, repr(argv))
            self.assertNotIn(secret, output.getvalue())
            self.assertEqual(payload['jit'], secret)
            self.assertTrue(payload['known_hosts'].endswith('-vm-123'))

    def test_timeout_reports_unknown_without_printing_exception(self):
        with (patch.object(transport.sys, 'argv', self.arguments('status')),
              patch.object(transport.sys, 'stdout', io.StringIO()) as output,
              patch.object(transport.subprocess, 'run', side_effect=subprocess.TimeoutExpired('secret', 75))):
            self.assertEqual(transport.main(), 1)
            self.assertEqual(json.loads(output.getvalue()), {'state': 'unknown'})

    def test_outer_ssh_refuses_agent_forwarding_to_metal(self):
        with (patch.object(transport.sys, 'argv', self.arguments('status')),
              patch.object(transport.sys, 'stdout', io.StringIO()),
              patch.object(transport.subprocess, 'run') as run):
            run.return_value = subprocess.CompletedProcess([], 0, '{"state":"installed"}')
            self.assertEqual(transport.main(), 0)
            argv = run.call_args.args[0]
            # Command-line -o is parsed before ~/.ssh/config, so the operator's own
            # ForwardAgent yes cannot enable forwarding across this hop.
            self.assertEqual(argv[argv.index('ForwardAgent=no') - 1], '-o')
            self.assertNotIn('ForwardAgent=yes', argv)

    def test_host_program_ssh_pins_guest_key_and_blocks_forwarded_agent(self):
        request = {'vm_id': 'vm-123', 'key_file': '/keys/admin',
                   'known_hosts': '/keys/known-vm-123', 'guest_program': transport.GUEST_PROGRAM}
        with (patch.object(transport.sys, 'stdin', io.StringIO(json.dumps(request))),
              patch.object(transport.sys, 'stdout', io.StringIO()) as output,
              patch.object(transport.subprocess, 'run') as run):
            run.return_value = subprocess.CompletedProcess([], 0, '{"state":"running"}')
            with self.assertRaises(SystemExit) as stopped:
                exec(transport.HOST_PROGRAM, {})
            self.assertEqual(stopped.exception.code, 0)
            self.assertEqual(output.getvalue(), '{"state":"running"}')
            argv = run.call_args.args[0]
            # The netns and destination are unchanged; only agent trust is tightened.
            self.assertEqual(argv[:5], ['ip', 'netns', 'exec', 'metal-vm-123', 'ssh'])
            self.assertEqual(argv[argv.index('-i') + 1], '/keys/admin')
            self.assertEqual(argv[argv.index('UserKnownHostsFile=/keys/known-vm-123') - 1], '-o')
            self.assertIn('root@172.16.0.2', argv)
            for option in ('ForwardAgent=no', 'IdentitiesOnly=yes'):
                self.assertEqual(argv[argv.index(option) - 1], '-o', option)
            self.assertNotIn('ForwardAgent=yes', argv)
            self.assertNotIn('IdentitiesOnly=no', argv)

    def test_guest_marker_without_started_service_is_not_completed(self):
        with tempfile.TemporaryDirectory() as directory:
            marker = Path(directory) / 'bootstrap.json'
            marker.write_text(json.dumps({'runner_name': 'runway-lease-123'}))
            program = transport.GUEST_PROGRAM.replace('/var/lib/runway/bootstrap.json', str(marker))
            for report, expected in [
                ('LoadState=not-found\nActiveState=inactive\nExecMainStartTimestampMonotonic=0\n', 'unknown'),
                ('LoadState=loaded\nActiveState=inactive\nExecMainStartTimestampMonotonic=123\n', 'completed'),
                ('LoadState=loaded\nActiveState=active\nExecMainStartTimestampMonotonic=123\n', 'running'),
            ]:
                with (self.subTest(expected=expected),
                      patch.object(transport.sys, 'stdin', io.StringIO(json.dumps({'action': 'status', 'runner_name': 'runway-lease-123'}))),
                      patch.object(transport.sys, 'stdout', io.StringIO()) as output,
                      patch.object(transport.subprocess, 'run', return_value=subprocess.CompletedProcess([], 0, report))):
                    with self.assertRaises(SystemExit) as stopped:
                        exec(program, {})
                    self.assertEqual(stopped.exception.code, 0)
                    self.assertEqual(json.loads(output.getvalue())['state'], expected)

    def test_collect_writes_private_file_without_printing_job_output(self):
        with tempfile.TemporaryDirectory() as directory:
            args = self.arguments('collect') + ['--diagnostics-dir', directory]
            secret = 'private-job-output'
            with (patch.object(transport.sys, 'argv', args),
                  patch.object(transport.sys, 'stdout', io.StringIO()) as output,
                  patch.object(transport.subprocess, 'run', return_value=subprocess.CompletedProcess(
                      [], 0, json.dumps({'state': 'completed', 'diagnostics': secret})))):
                self.assertEqual(transport.main(), 0)
                self.assertNotIn(secret, output.getvalue())
                log = Path(directory) / 'runway-lease-123-vm-123.log'
                self.assertEqual(log.read_text(), secret)
                self.assertEqual(log.stat().st_mode & 0o777, 0o600)

    def test_expired_deadline_is_rejected_before_ssh(self):
        args = self.arguments() + ['--deadline', '2000-01-01T00:00:00+00:00']
        with (patch.object(transport.sys, 'argv', args),
              patch.object(transport.sys, 'stderr', io.StringIO()),
              patch.object(transport.subprocess, 'run') as run,
              self.assertRaises(SystemExit)):
            transport.main()
        run.assert_not_called()

    def test_vm_shell_injection_is_rejected_before_ssh(self):
        args = self.arguments('status')
        args[args.index('--vm-id') + 1] = 'vm-123; echo unsafe'
        with (patch.object(transport.sys, 'argv', args),
              patch.object(transport.sys, 'stderr', io.StringIO()),
              patch.object(transport.subprocess, 'run') as run,
              self.assertRaises(SystemExit)):
            transport.main()
        run.assert_not_called()

    def test_sudo_opt_in_is_explicit_and_carried_in_the_payload(self):
        for flag, expected in (([], False), (['--allow-guest-sudo'], True)):
            with self.subTest(flag=flag):
                with (patch.object(transport.sys, 'argv', self.arguments('prepare') + flag),
                      patch.object(transport.sys, 'stdout', io.StringIO()),
                      patch.object(transport.subprocess, 'run') as run):
                    run.return_value = subprocess.CompletedProcess([], 0, '{"state":"installed"}')
                    self.assertEqual(transport.main(), 0)
                payload = json.loads(run.call_args.kwargs['input'])
                self.assertIs(payload['allow_guest_sudo'], expected)

    def test_prepare_without_opt_in_checks_only_and_writes_nothing(self):
        with tempfile.TemporaryDirectory() as directory:
            program, sudoers = self.guest_paths(directory)
            code, output, recorded, ownership = self.execute_guest(
                program, sudoers, {'action': 'prepare', 'runner_name': 'runway-lease-123'})
            self.assertEqual(code, 0)
            self.assertEqual(json.loads(output), {'state': 'installed'})
            self.assertEqual(list(sudoers.parent.iterdir()), [])
            self.assertEqual(ownership, [])
            self.assertEqual([argv for argv in recorded if any('sudo' in part for part in argv)], [])

    def test_opt_in_prepare_installs_a_restrictive_rule_and_verifies_it(self):
        with tempfile.TemporaryDirectory() as directory:
            program, sudoers = self.guest_paths(directory)
            payload = {'action': 'prepare', 'runner_name': 'runway-lease-123', 'allow_guest_sudo': True}
            code, output, recorded, ownership = self.execute_guest(program, sudoers, payload)
            self.assertEqual(code, 0)
            self.assertEqual(json.loads(output), {'state': 'installed'})
            self.assertEqual(sudoers.read_text(), SUDOERS_RULE)
            # sudo(8) skips a drop-in that group or world can write, so the dedicated
            # rule must land as a plain root-owned 0440 file.
            self.assertEqual(stat.S_IMODE(sudoers.lstat().st_mode), 0o440)
            self.assertEqual(ownership, [(0, 0)])
            validation = ['/usr/bin/visudo', '-c', '-f', str(sudoers)]
            proof = ['runuser', '-u', 'runner', '--', '/usr/bin/sudo', '-n', 'true']
            self.assertIn(validation, recorded)
            self.assertIn(proof, recorded)
            # Parsing alone is not proof: the runner account must reach root without a
            # password before preparation reports the guest ready.
            self.assertLess(recorded.index(validation), recorded.index(proof))
            # Nothing is installed or downloaded; only the baked sudo binaries are used.
            self.assertEqual([argv for argv in recorded
                              if any(part in ('apt', 'apt-get', 'curl', 'wget') for part in argv)], [])
            # A controller that died before recording the phase prepares the same guest
            # again: the identical drop-in is re-validated, not rewritten or refused.
            repeated, _, repeated_calls, _ = self.execute_guest(program, sudoers, payload)
            self.assertEqual(repeated, 0)
            self.assertEqual(sudoers.read_text(), SUDOERS_RULE)
            self.assertIn(validation, repeated_calls)

    def test_opt_in_prepare_fails_closed_when_sudo_is_absent_or_unproven(self):
        for absent, broken in [(('sudo',), None), (('visudo',), None),
                               ((), 'validation'), ((), 'passwordless')]:
            with self.subTest(absent=absent, broken=broken):
                with tempfile.TemporaryDirectory() as directory:
                    program, sudoers = self.guest_paths(directory)
                    results = {}
                    if broken == 'validation':
                        results[('/usr/bin/visudo', '-c', '-f', str(sudoers))] = (1, '')
                    if broken == 'passwordless':
                        results[('runuser', '-u', 'runner', '--', '/usr/bin/sudo', '-n', 'true')] = (1, '')
                    present = tuple(binary for binary in ('sudo', 'visudo') if binary not in absent)
                    code, output, _, _ = self.execute_guest(
                        program, sudoers,
                        {'action': 'prepare', 'runner_name': 'runway-lease-123', 'allow_guest_sudo': True},
                        present=present, results=results)
                    # Preparation is refused, so no bootstrap follows it on this guest.
                    self.assertEqual(code, 1)
                    self.assertEqual(json.loads(output), {'state': 'unknown'})

    def test_opt_in_never_configures_a_non_prepare_action(self):
        for action, expected in (('status', 0), ('collect', 0), ('stop', 0), ('bootstrap', 1)):
            with self.subTest(action=action):
                with tempfile.TemporaryDirectory() as directory:
                    program, sudoers = self.guest_paths(directory)
                    code, _, recorded, ownership = self.execute_guest(
                        program, sudoers,
                        {'action': action, 'runner_name': 'runway-lease-123', 'allow_guest_sudo': True})
                    self.assertEqual(code, expected)
                    self.assertEqual(list(sudoers.parent.iterdir()), [])
                    self.assertEqual(ownership, [])
                    self.assertEqual([argv for argv in recorded if any('sudo' in part for part in argv)], [])

    def test_opt_in_never_configures_a_registered_guest(self):
        for report, registered, expected in [(RUNNING_REPORT, 'marker', 'running'),
                                             (COMPLETED_REPORT, 'marker', 'completed'),
                                             (None, 'dot-runner', 'unknown')]:
            with self.subTest(registered=registered, expected=expected):
                with tempfile.TemporaryDirectory() as directory:
                    program, sudoers = self.guest_paths(directory)
                    results = {SERVICE_REPORT: (0, report)} if report else {}
                    if registered == 'marker':
                        (Path(directory) / 'bootstrap.json').write_text(
                            json.dumps({'runner_name': 'runway-lease-123'}))
                    else:
                        (Path(directory) / 'actions-runner/.runner').write_text('{}')
                    code, output, recorded, ownership = self.execute_guest(
                        program, sudoers,
                        {'action': 'prepare', 'runner_name': 'runway-lease-123', 'allow_guest_sudo': True},
                        results=results)
                    self.assertEqual(code, 1)
                    self.assertEqual(json.loads(output), {'state': expected})
                    self.assertEqual(list(sudoers.parent.iterdir()), [])
                    self.assertEqual(ownership, [])
                    self.assertEqual([argv for argv in recorded if any('sudo' in part for part in argv)], [])

    def test_opt_in_refuses_a_planted_drop_in_and_revalidates_an_identical_one(self):
        for kind, expected_code in (('symlink', 1), ('foreign-rule', 1),
                                    ('world-readable', 1), ('identical', 0)):
            with self.subTest(kind=kind):
                with tempfile.TemporaryDirectory() as directory:
                    program, sudoers = self.guest_paths(directory)
                    target = Path(directory, 'planted')
                    if kind == 'symlink':
                        target.write_text('root ALL=(ALL) NOPASSWD: ALL\n')
                        sudoers.symlink_to(target)
                        planted = target.read_text()
                    else:
                        body = SUDOERS_RULE if kind in ('world-readable', 'identical') \
                            else 'runner ALL=(ALL) PASSWD: ALL\n'
                        sudoers.write_text(body)
                        sudoers.chmod(0o644 if kind == 'world-readable' else 0o440)
                        planted = body
                    code, _, recorded, _ = self.execute_guest(
                        program, sudoers,
                        {'action': 'prepare', 'runner_name': 'runway-lease-123', 'allow_guest_sudo': True})
                    self.assertEqual(code, expected_code)
                    # A file this code did not create is never rewritten, truncated or
                    # followed, whatever an earlier attempt left at that path.
                    self.assertEqual(target.read_text() if kind == 'symlink' else sudoers.read_text(), planted)
                    self.assertEqual(sudoers.is_symlink(), kind == 'symlink')
                    sudo_calls = [argv for argv in recorded if any('sudo' in part for part in argv)]
                    if expected_code:
                        # Refused before sudo(8) is ever pointed at an untrusted file.
                        self.assertEqual(sudo_calls, [])
                    else:
                        self.assertIn(['runuser', '-u', 'runner', '--', '/usr/bin/sudo', '-n', 'true'],
                                      sudo_calls)

    def test_opt_in_create_refuses_a_drop_in_planted_after_the_check(self):
        with tempfile.TemporaryDirectory() as directory:
            program, sudoers = self.guest_paths(directory)
            target = Path(directory, 'planted')
            target.write_text('root ALL=(ALL) NOPASSWD: ALL\n')
            planted = []

            def plant():
                if not planted:
                    planted.append(True)
                    sudoers.symlink_to(target)

            code, output, recorded, _ = self.execute_guest(
                program, sudoers,
                {'action': 'prepare', 'runner_name': 'runway-lease-123', 'allow_guest_sudo': True},
                plant=plant)
            # O_EXCL|O_NOFOLLOW closes the window between the check and the create, so
            # a drop-in that appears there is never written through or trusted.
            self.assertEqual(code, 1)
            self.assertEqual(json.loads(output), {'state': 'unknown'})
            self.assertEqual(target.read_text(), 'root ALL=(ALL) NOPASSWD: ALL\n')
            self.assertEqual([argv for argv in recorded if any('sudo' in part for part in argv)], [])


if __name__ == '__main__':
    unittest.main()
