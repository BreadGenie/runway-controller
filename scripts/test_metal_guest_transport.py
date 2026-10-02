"""Verify that the administrative transport keeps JIT material out of argv."""

import importlib.util
import io
import json
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


class TransportTests(unittest.TestCase):
    def arguments(self, action='bootstrap'):
        return ['transport', '--host', 'root@metal.test', '--key-file', '/keys/admin',
                '--known-hosts', '/keys/known', action, '--vm-id', 'vm-123',
                '--runner-name', 'runway-lease-123']

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


if __name__ == '__main__':
    unittest.main()
