"""Verify that the administrative transport keeps JIT material out of argv."""

import builtins
import importlib.util
import io
import json
import os
import shlex
import shutil
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
# A healthy 6 vCPU guest of the shape the trial measured: 6 GiB total, 5.1 GB available.
MEMINFO = 'MemTotal:       6168576 kB\nMemFree:        4103352 kB\nMemAvailable:   5222412 kB\n'


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
        (root / 'docker').mkdir(parents=True, exist_ok=True)
        # The cache directory is deliberately left out: sealing empties the caches, so a
        # fresh guest arrives without one and preparation has to create it.
        program = transport.GUEST_PROGRAM
        for guest_path, private_path in (('/var/lib/runway/bootstrap.json', root / 'bootstrap.json'),
                                         ('/home/runner/actions-runner', root / 'actions-runner'),
                                         ('/home/runner/.cache', root / 'cache'),
                                         ('/etc/sudoers.d/runway-runner', root / 'sudoers/runway-runner'),
                                         ('/etc/sudoers.d', root / 'sudoers'),
                                         ('/etc/docker/daemon.json', root / 'docker/daemon.json')):
            program = program.replace(guest_path, str(private_path))
        # Every file this code may create is root-owned in the guest. The test user
        # cannot own them, so the recorded ownership stands in for that.
        return program, root / 'sudoers/runway-runner', [root / 'sudoers/runway-runner',
                                                        root / 'docker/daemon.json']

    def execute_guest(self, program, sudoers, payload, *, present=('sudo', 'visudo'),
                      results=None, plant=None, root_owned=(), meminfo=None):
        """Run the embedded guest program locally: no SSH and no real guest binary."""
        real_lstat = os.lstat
        real_open = builtins.open
        recorded, ownership = [], []
        # A probe this program issues more than once in a single run is answered in
        # order, so the mount table read before a mount can differ from the one after.
        results = {key: list(value) if isinstance(value, list) else value
                   for key, value in (results or {}).items()}
        owned = {os.fspath(path) for path in (list(root_owned) + [sudoers])}

        def fake_run(argv, **kwargs):
            recorded.append(list(argv))
            answers = results.get(tuple(argv), (0, ''))
            if isinstance(answers, list):
                answers = answers.pop(0) if answers else (1, '')
            return subprocess.CompletedProcess(list(argv), *answers)

        def fake_which(name, path=None, **kwargs):
            return '/usr/bin/' + name if name in present else None

        def fake_fchown(descriptor, uid, gid):
            # An unprivileged test user cannot hand a temporary file to root: record
            # the ownership the guest program demands instead of granting it.
            ownership.append((uid, gid))

        def fake_open(path, *rest, **options):
            # The memory reserve is read from procfs, which this unprivileged test
            # cannot dictate. Only that one file is answered here.
            if path == '/proc/meminfo':
                return io.StringIO(MEMINFO if meminfo is None else meminfo)
            return real_open(path, *rest, **options)

        def fake_lstat(path):
            if os.fspath(path) not in owned:
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
              patch('builtins.open', side_effect=fake_open),
              patch.object(transport.os, 'fchown', side_effect=fake_fchown),
              patch.object(transport.os, 'lstat', side_effect=fake_lstat),
              patch.object(transport.subprocess, 'run', side_effect=fake_run)):
            with self.assertRaises(SystemExit) as stopped:
                exec(program, {})
        return stopped.exception.code, output.getvalue(), recorded, ownership

    def host_request(self, directory, action='prepare', port=5000):
        return {'action': action, 'vm_id': 'vm-123', 'key_file': '/keys/admin',
                'known_hosts': '/keys/known-vm-123', 'guest_program': transport.GUEST_PROGRAM,
                'forward_dir': str(Path(directory) / 'runway-forward-vm-123'),
                'mirror_port': port}

    def execute_host(self, request, *, alive=True, exit_closes=True, start=0,
                     probe='{"registry": true}', guest='{"state": "installed"}'):
        """Run the embedded host program against a modelled control master.

        The master has a life of its own here: `alive` is whether one is already
        listening on the control socket, and `exit_closes` is whether it honours
        `-O exit`. Each of the four commands the host program can issue is told apart by
        a marker belonging to that command alone: a control verb, the master flag, the
        probe's proxy opener and the guest program's own helper name.
        """
        recorded = []
        master = {'alive': alive}

        def fake_run(argv, **kwargs):
            recorded.append(list(argv))
            last = argv[-1]
            if '-O' in argv:
                if 'exit' in argv and exit_closes:
                    master['alive'] = False
                    return subprocess.CompletedProcess(list(argv), 0, '')
                # ssh(1) reports a dead master on stderr with a non-zero status.
                return subprocess.CompletedProcess(list(argv), 0 if master['alive'] else 255, '')
            if '-M' in argv:
                if start == 0:
                    master['alive'] = True
                return subprocess.CompletedProcess(list(argv), start, '')
            if 'ProxyHandler' in last:
                return subprocess.CompletedProcess(list(argv), 0, probe)
            if 'grant_runner_sudo' in last:
                return subprocess.CompletedProcess(list(argv), 0, guest)
            return subprocess.CompletedProcess(list(argv), 0, guest)

        with (patch.object(transport.sys, 'stdin', io.StringIO(json.dumps(request))),
              patch.object(transport.sys, 'stdout', io.StringIO()) as output,
              patch('shutil.which', side_effect=lambda name, path=None, **kw: '/usr/bin/' + name),
              patch.object(transport.subprocess, 'run', side_effect=fake_run)):
            with self.assertRaises(SystemExit) as stopped:
                exec(transport.HOST_PROGRAM, {})
        return stopped.exception.code, output.getvalue(), recorded

    @staticmethod
    def forwards(recorded):
        """The control-master invocations that own the reverse forward's lifetime."""
        return [argv for argv in recorded if '-M' in argv]

    @staticmethod
    def guests(recorded):
        """The invocations that run code inside the guest, forward probe excluded."""
        return [argv for argv in recorded if 'ssh' in argv and 'ProxyHandler' not in argv[-1]
                and not {'-O', '-M'} & set(argv)]

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
        with tempfile.TemporaryDirectory() as directory:
            code, output, recorded = self.execute_host(
                self.host_request(directory), guest='{"state":"running"}')
            self.assertEqual(code, 0)
            self.assertEqual(output, '{"state":"running"}')
            argv, = self.guests(recorded)
            # The destination and key are unchanged; only agent trust is tightened.
            self.assertEqual(argv[0], 'ssh')
            self.assertEqual(argv[argv.index('-i') + 1], '/keys/admin')
            self.assertEqual(argv[argv.index('UserKnownHostsFile=/keys/known-vm-123') - 1], '-o')
            self.assertIn('root@172.16.0.2', argv)
            for option in ('ForwardAgent=no', 'IdentitiesOnly=yes'):
                self.assertEqual(argv[argv.index(option) - 1], '-o', option)
            self.assertNotIn('ForwardAgent=yes', argv)
            self.assertNotIn('IdentitiesOnly=no', argv)

    def test_guest_command_stays_in_the_root_namespace_and_proxies_into_it(self):
        with tempfile.TemporaryDirectory() as directory:
            _, _, recorded = self.execute_host(self.host_request(directory))
            argv, = self.guests(recorded)
            proxy, = [part for part in argv if part.startswith('ProxyCommand=')]
            # The client process is the one that receives a reverse forward's local end.
            # Running it inside the guest's namespace would deliver to that namespace's
            # loopback instead of Metal's, so the client stays out and only the TCP
            # connection is proxied in.
            self.assertEqual(argv[0], 'ssh')
            self.assertNotIn('netns', argv)
            self.assertEqual(shlex.split(proxy[len('ProxyCommand='):]),
                             ['ip', 'netns', 'exec', 'metal-vm-123', 'socat', '-',
                              'TCP:172.16.0.2:22'])

    def test_reverse_forward_maps_the_guest_loopback_onto_this_host_loopback(self):
        with tempfile.TemporaryDirectory() as directory:
            _, _, recorded = self.execute_host(self.host_request(directory), alive=False)
            argv, = self.forwards(recorded)
            # -R is the whole delivery mapping: the guest's sshd listens on its own
            # loopback port and hands the connection back to this client, whose loopback
            # is Metal's because the client never left the root namespace.
            self.assertEqual(argv[argv.index('-R') + 1], '127.0.0.1:5000:127.0.0.1:5000')
            self.assertEqual(argv[argv.index('-S') + 1],
                             str(Path(directory) / 'runway-forward-vm-123/forward.sock'))
            self.assertIn('root@172.16.0.2', argv)
            # A control master is the forward's durable owner, so the socket is how a
            # later action reuses or closes it. ControlPersist would outlive the guest
            # and strand it on the host; ExitOnForwardFailure keeps a refused bind from
            # reporting success while the job quietly pays full Docker Hub latency.
            self.assertIn('-M', argv)
            self.assertIn('-f', argv)
            self.assertIn('-N', argv)
            self.assertEqual(argv[argv.index('ControlPersist=no') - 1], '-o')
            self.assertEqual(argv[argv.index('ExitOnForwardFailure=yes') - 1], '-o')
            # A master owns a forward, so it must also notice a guest that stopped
            # answering: ControlPersist=no only releases it once the connection is
            # already gone, which nothing here detects.
            for option in ('ServerAliveInterval=10', 'ServerAliveCountMax=3'):
                self.assertEqual(argv[argv.index(option) - 1], '-o', option)

    def test_every_control_verb_names_the_host_whose_socket_it_means(self):
        with tempfile.TemporaryDirectory() as directory:
            _, _, recorded = self.execute_host(self.host_request(directory, action='stop'),
                                               alive=True)
            verbs = [argv for argv in recorded if '-O' in argv]
            self.assertTrue(verbs)
            # ssh(1) rejects a control verb that names no destination, so a control
            # command without one reports usage and never reaches the socket. That
            # would make every check report no master and every stop delete the
            # control socket of a live one.
            for argv in verbs:
                self.assertEqual(argv[-1], 'root@172.16.0.2')
                self.assertIn('-S', argv)
            self.assertEqual({argv[argv.index('-O') + 1] for argv in verbs}, {'check', 'exit'})

    def test_repeated_prepare_reuses_one_forward_instead_of_stacking_it(self):
        with tempfile.TemporaryDirectory() as directory:
            _, _, recorded = self.execute_host(self.host_request(directory), alive=False)
            self.assertEqual(len(self.forwards(recorded)), 1)
            # A second prepare of the same clone finds the master alive and the guest
            # already reaching a registry, so it must not open a second listener on the
            # guest's port, which the first forward already owns.
            _, _, again = self.execute_host(self.host_request(directory), alive=True)
            self.assertEqual(self.forwards(again), [])
            self.assertEqual([argv for argv in again if 'exit' in argv], [])

    def test_prepare_refuses_when_the_guest_cannot_reach_a_registry(self):
        for probe, expected in [('{"registry": false}', 1), ('', 1), ('not json', 1)]:
            with self.subTest(probe=probe):
                with tempfile.TemporaryDirectory() as directory:
                    code, output, recorded = self.execute_host(
                        self.host_request(directory), alive=False, probe=probe)
                    # Preparation fails closed, so no JIT configuration is requested and
                    # no bootstrap follows on this guest.
                    self.assertEqual(code, expected)
                    self.assertEqual(output, '')
                    self.assertEqual(self.guests(recorded), [])

    def test_start_failure_is_not_reported_as_a_working_mirror(self):
        with tempfile.TemporaryDirectory() as directory:
            code, _, recorded = self.execute_host(
                self.host_request(directory), alive=False, start=255)
            self.assertEqual(code, 1)
            self.assertEqual(self.guests(recorded), [])

    def test_stop_closes_the_forward_and_removes_its_control_socket(self):
        with tempfile.TemporaryDirectory() as directory:
            forward = Path(directory) / 'runway-forward-vm-123'
            code, output, recorded = self.execute_host(
                self.host_request(directory, action='stop'), alive=True)
            self.assertEqual(code, 0)
            # Teardown must not swallow the guest's own answer: the controller parses
            # this stream, so the forward is closed without replacing it.
            self.assertEqual(output, '{"state": "installed"}')
            exits = [argv for argv in recorded if 'exit' in argv]
            self.assertEqual(len(exits), 1)
            # Teardown proves the master is gone instead of assuming the exit landed.
            checks = [argv for argv in recorded if 'check' in argv]
            self.assertGreaterEqual(len(checks), 2)
            self.assertFalse(forward.exists())
            # A master that will not close is reported, not waited on forever.
            code, _, _ = self.execute_host(
                self.host_request(directory, action='stop'), alive=True, exit_closes=False)
            self.assertEqual(code, 1)

    def test_an_ordinary_action_never_touches_the_forward(self):
        for action in ('status', 'bootstrap', 'collect'):
            with self.subTest(action=action):
                with tempfile.TemporaryDirectory() as directory:
                    code, _, recorded = self.execute_host(
                        self.host_request(directory, action=action), alive=True)
                    self.assertEqual(code, 0)
                    # The forward is opened by prepare and closed by stop. status,
                    # bootstrap and collect run while a job is live and must not close
                    # the mirror the running job is pulling through.
                    self.assertEqual(self.forwards(recorded), [])
                    self.assertEqual([argv for argv in recorded if '-O' in argv], [])
                    self.assertEqual(len(self.guests(recorded)), 1)

    def test_without_the_flag_nothing_is_forwarded_or_configured(self):
        with tempfile.TemporaryDirectory() as directory:
            request = self.host_request(directory)
            del request['mirror_port']
            code, _, recorded = self.execute_host(request, alive=False)
            self.assertEqual(code, 0)
            self.assertEqual(self.forwards(recorded), [])
            self.assertEqual([argv for argv in recorded if '-O' in argv], [])
            # With no port there is nothing to forward and no port to hand the guest
            # program, so the guest command is the only one issued at all.
            self.assertEqual(len(recorded), 1)
            self.assertNotIn('-R', recorded[0])
            self.assertEqual(recorded[0][:5], ['ip', 'netns', 'exec', 'metal-vm-123', 'ssh'])
            self.assertFalse(any(option.startswith('ProxyCommand=') for option in recorded[0]))

    def test_the_flag_is_carried_in_the_payload_with_a_private_control_directory(self):
        for port in (5000, None):
            with self.subTest(port=port):
                args = self.arguments('prepare') + (['--registry-mirror-port', str(port)]
                                                    if port else [])
                with (patch.object(transport.sys, 'argv', args),
                      patch.object(transport.sys, 'stdout', io.StringIO()),
                      patch.object(transport.subprocess, 'run') as run):
                    run.return_value = subprocess.CompletedProcess([], 0, '{"state":"installed"}')
                    self.assertEqual(transport.main(), 0)
                payload = json.loads(run.call_args.kwargs['input'])
                self.assertEqual(payload.get('mirror_port'), port)
                # The socket lives beside the known-hosts prefix the controller already
                # reserves, one private directory per VM.
                self.assertEqual(payload.get('forward_dir'),
                                 '/keys/runway-forward-vm-123' if port else None)

    def test_the_control_directory_is_private_and_never_reused_when_shared(self):
        with tempfile.TemporaryDirectory() as directory:
            _, _, recorded = self.execute_host(self.host_request(directory))
            forward = Path(directory) / 'runway-forward-vm-123'
            # A control directory another account can read would let a stranger address
            # this VM's master and its forward.
            self.assertEqual(stat.S_IMODE(forward.lstat().st_mode), 0o700)
            forward.chmod(0o755)
            code, _, recorded = self.execute_host(self.host_request(directory))
            self.assertEqual(code, 1)
            self.assertEqual(self.forwards(recorded), [])
            # A symlink at that path is refused rather than followed: os.mkdir would
            # otherwise fail and os.lstat would report the target's mode.
            shutil.rmtree(forward)
            target = Path(directory) / 'elsewhere'
            target.mkdir(mode=0o700)
            forward.symlink_to(target)
            code, _, _ = self.execute_host(self.host_request(directory))
            self.assertEqual(code, 1)

    def test_mirror_port_outside_the_unprivileged_range_is_rejected(self):
        for port in ('80', '1023', '65536', '0', '-1'):
            with self.subTest(port=port):
                args = self.arguments('prepare') + ['--registry-mirror-port', port]
                with (patch.object(transport.sys, 'argv', args),
                      patch.object(transport.sys, 'stderr', io.StringIO()),
                      patch.object(transport.subprocess, 'run') as run,
                      self.assertRaises(SystemExit)):
                    transport.main()
                run.assert_not_called()

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

    def place_daemon_config(self, directory, text):
        """Put a daemon.json in the private tree, creating its directory first.

        The mode is set explicitly because this user's umask is 0002, which would leave
        a group-writable file and preparation would refuse it for the right reason.
        """
        (Path(directory) / 'docker').mkdir(parents=True, exist_ok=True)
        config = Path(directory) / 'docker/daemon.json'
        config.write_text(text)
        config.chmod(0o644)
        return config

    def mirror_guest(self, directory, results=None):
        """Prepare a fresh guest with the mirror opt-in and the guest's own answers."""
        program, sudoers, owned = self.guest_paths(directory)
        payload = {'action': 'prepare', 'runner_name': 'runway-lease-123', 'mirror_port': 5000}
        return (self.execute_guest(program, sudoers, payload, root_owned=owned,
                                   present=('dockerd', 'docker'), results=results)
                + (program, sudoers, owned))

    @staticmethod
    def reported(value):
        return json.dumps([value])

    def test_mirror_merges_into_the_daemon_configuration_without_losing_anything(self):
        with tempfile.TemporaryDirectory() as directory:
            existing = {'log-driver': 'json-file', 'registry-mirrors': ['http://10.0.0.1:5000/']}
            self.place_daemon_config(directory, json.dumps(existing))
            code, output, recorded, ownership, _, _, _ = self.mirror_guest(
                directory, results={
                    ('/usr/bin/docker', 'info', '--format',
                     '{{json .RegistryConfig.Mirrors}}'): (
                         0, self.reported('http://127.0.0.1:5000/'))})
            self.assertEqual(code, 0)
            self.assertEqual(json.loads(output), {'state': 'installed'})
            document = json.loads((Path(directory) / 'docker/daemon.json').read_text())
            # Every pre-existing key survives: this merge owns two keys and no others.
            self.assertEqual(document['log-driver'], 'json-file')
            self.assertEqual(document['registry-mirrors'],
                             ['http://10.0.0.1:5000/', 'http://127.0.0.1:5000/'])
            # insecure-registry takes a bare host:port, never a scheme, and the daemon
            # stores a mirror with a trailing slash. Both forms are load-bearing.
            self.assertEqual(document['insecure-registries'], ['127.0.0.1:5000'])
            self.assertEqual(ownership, [(0, 0)])
            validate = [argv for argv in recorded if '--validate' in argv]
            restart = [argv for argv in recorded if argv[-3:] == ['systemctl', 'restart', 'docker']]
            # The file is validated before the daemon is pointed at it, then the daemon
            # is restarted, and only a mirror it reports counts as configured.
            self.assertEqual(len(validate), 1)
            self.assertEqual(len(restart), 1)
            self.assertLess(recorded.index(validate[0]), recorded.index(restart[0]))
            report = [argv for argv in recorded if 'RegistryConfig' in argv[-1]]
            self.assertEqual(len(report), 1)
            self.assertEqual(report[0][1], 'info')
            self.assertLess(recorded.index(restart[0]), recorded.index(report[0]))
            # A mirror must not become a pre-pulled local store: that would hide the
            # very latency this measures.
            self.assertEqual([argv for argv in recorded if 'pull' in argv], [])

    def test_repeated_preparation_does_not_stack_the_mirror(self):
        with tempfile.TemporaryDirectory() as directory:
            answers = {('/usr/bin/docker', 'info', '--format',
                        '{{json .RegistryConfig.Mirrors}}'): (
                            0, self.reported('http://127.0.0.1:5000/'))}
            self.mirror_guest(directory, results=answers)
            self.mirror_guest(directory, results=answers)
            document = json.loads((Path(directory) / 'docker/daemon.json').read_text())
            self.assertEqual(document['registry-mirrors'], ['http://127.0.0.1:5000/'])
            self.assertEqual(document['insecure-registries'], ['127.0.0.1:5000'])

    def test_mirror_is_undone_when_the_daemon_does_not_report_it(self):
        for broken in ('absent-mirror', 'unparsable', 'empty', 'failed'):
            with self.subTest(broken=broken):
                with tempfile.TemporaryDirectory() as directory:
                    before = {'log-driver': 'local'}
                    self.place_daemon_config(directory, json.dumps(before))
                    answers = {('/usr/bin/docker', 'info', '--format',
                                '{{json .RegistryConfig.Mirrors}}'): (0, '')}
                    if broken == 'absent-mirror':
                        answers[('/usr/bin/docker', 'info', '--format',
                                 '{{json .RegistryConfig.Mirrors}}')] = (0, '[]')
                    if broken == 'unparsable':
                        answers[('/usr/bin/docker', 'info', '--format',
                                 '{{json .RegistryConfig.Mirrors}}')] = (0, 'not json')
                    if broken == 'failed':
                        answers[('/usr/bin/docker', 'info', '--format',
                                 '{{json .RegistryConfig.Mirrors}}')] = (1, '')
                    code, output, recorded, _, _, _, _ = self.mirror_guest(
                        directory, results=answers)
                    # A mirror the job would not use is worse than none, so preparation
                    # fails closed and restores the configuration it found.
                    self.assertEqual(code, 1)
                    self.assertEqual(json.loads(output), {'state': 'unknown'})
                    self.assertEqual(json.loads(
                        (Path(directory) / 'docker/daemon.json').read_text()), before)
                    # Docker is left running on the restored file, not on the rejected one.
                    restarts = [argv for argv in recorded
                                if argv[-3:] == ['systemctl', 'restart', 'docker']]
                    self.assertEqual(len(restarts), 2)

    def test_mirror_never_reaches_a_registered_guest(self):
        for report, expected in [(RUNNING_REPORT, 'running'), (COMPLETED_REPORT, 'completed')]:
            with self.subTest(expected=expected):
                with tempfile.TemporaryDirectory() as directory:
                    program, sudoers, owned = self.guest_paths(directory)
                    (Path(directory) / 'bootstrap.json').write_text(
                        json.dumps({'runner_name': 'runway-lease-123'}))
                    code, output, recorded, ownership = self.execute_guest(
                        program, sudoers,
                        {'action': 'prepare', 'runner_name': 'runway-lease-123', 'mirror_port': 5000},
                        present=('dockerd', 'docker'), results={SERVICE_REPORT: (0, report)},
                        root_owned=owned)
                    self.assertEqual(code, 1)
                    self.assertEqual(json.loads(output), {'state': expected})
                    self.assertFalse((Path(directory) / 'docker/daemon.json').exists())
                    # Only the state read runs: no configuration, no daemon restart.
                    self.assertEqual([argv for argv in recorded
                                      if argv[-3:] == ['systemctl', 'restart', 'docker']], [])
                    self.assertEqual([argv for argv in recorded if '--validate' in argv], [])

    def test_mirror_refuses_a_daemon_file_this_code_did_not_write(self):
        for kind, expected in (('symlink', 1), ('world-writable', 1), ('unparsable', 1),
                               ('not-an-object', 1), ('foreign-key-type', 1)):
            with self.subTest(kind=kind):
                with tempfile.TemporaryDirectory() as directory:
                    program, sudoers, owned = self.guest_paths(directory)
                    target = Path(directory) / 'planted'
                    body = {'registry-mirrors': 'not-a-list'}
                    config = Path(directory) / 'docker/daemon.json'
                    # Every case but the world-writable one plants a file this code
                    # would otherwise accept, so each is refused for the reason named
                    # here rather than for its mode.
                    if kind == 'symlink':
                        target.write_text(json.dumps({'log-driver': 'local'}))
                        config.symlink_to(target)
                    elif kind == 'world-writable':
                        self.place_daemon_config(directory, json.dumps({'log-driver': 'local'}))
                        config.chmod(0o666)
                    elif kind == 'unparsable':
                        self.place_daemon_config(directory, '{ not json')
                    elif kind == 'not-an-object':
                        self.place_daemon_config(directory, '["a list"]')
                    else:
                        self.place_daemon_config(directory, json.dumps(body))
                    planted = target.read_text() if kind == 'symlink' else config.read_text()
                    code, output, recorded, _ = self.execute_guest(
                        program, sudoers,
                        {'action': 'prepare', 'runner_name': 'runway-lease-123', 'mirror_port': 5000},
                        present=('dockerd', 'docker'),
                        results={('/usr/bin/docker', 'info', '--format',
                                  '{{json .RegistryConfig.Mirrors}}'): (
                                      0, self.reported('http://127.0.0.1:5000/'))},
                        root_owned=owned)
                    self.assertEqual(code, expected)
                    self.assertEqual(json.loads(output)['state'], 'unknown')
                    # A file this code did not create is never rewritten or followed.
                    self.assertEqual(target.read_text() if kind == 'symlink'
                                     else config.read_text(), planted)
                    self.assertEqual(config.is_symlink(), kind == 'symlink')
                    self.assertEqual([argv for argv in recorded
                                      if argv[-3:] == ['systemctl', 'restart', 'docker']], [])

    def test_mirror_refuses_a_guest_without_docker(self):
        for absent in ('dockerd', 'docker'):
            with self.subTest(absent=absent):
                with tempfile.TemporaryDirectory() as directory:
                    program, sudoers, owned = self.guest_paths(directory)
                    present = tuple(binary for binary in ('dockerd', 'docker')
                                    if binary != absent)
                    code, output, _, _ = self.execute_guest(
                        program, sudoers,
                        {'action': 'prepare', 'runner_name': 'runway-lease-123', 'mirror_port': 5000},
                        present=present, root_owned=owned)
                    # The baked image must already carry Docker; nothing is installed.
                    self.assertEqual(code, 1)
                    self.assertEqual(json.loads(output), {'state': 'unknown'})
                    self.assertFalse((Path(directory) / 'docker/daemon.json').exists())

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
            program, sudoers, owned = self.guest_paths(directory)
            code, output, recorded, ownership = self.execute_guest(
                program, sudoers, {'action': 'prepare', 'runner_name': 'runway-lease-123'})
            self.assertEqual(code, 0)
            self.assertEqual(json.loads(output), {'state': 'installed'})
            self.assertEqual(list(sudoers.parent.iterdir()), [])
            self.assertEqual(ownership, [])
            self.assertEqual([argv for argv in recorded if any('sudo' in part for part in argv)], [])

    def test_opt_in_prepare_installs_a_restrictive_rule_and_verifies_it(self):
        with tempfile.TemporaryDirectory() as directory:
            program, sudoers, owned = self.guest_paths(directory)
            payload = {'action': 'prepare', 'runner_name': 'runway-lease-123', 'allow_guest_sudo': True}
            code, output, recorded, ownership = self.execute_guest(program, sudoers, payload, root_owned=owned)
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
            repeated, _, repeated_calls, _ = self.execute_guest(program, sudoers, payload, root_owned=owned)
            self.assertEqual(repeated, 0)
            self.assertEqual(sudoers.read_text(), SUDOERS_RULE)
            self.assertIn(validation, repeated_calls)

    def test_opt_in_prepare_fails_closed_when_sudo_is_absent_or_unproven(self):
        for absent, broken in [(('sudo',), None), (('visudo',), None),
                               ((), 'validation'), ((), 'passwordless')]:
            with self.subTest(absent=absent, broken=broken):
                with tempfile.TemporaryDirectory() as directory:
                    program, sudoers, owned = self.guest_paths(directory)
                    results = {}
                    if broken == 'validation':
                        results[('/usr/bin/visudo', '-c', '-f', str(sudoers))] = (1, '')
                    if broken == 'passwordless':
                        results[('runuser', '-u', 'runner', '--', '/usr/bin/sudo', '-n', 'true')] = (1, '')
                    present = tuple(binary for binary in ('sudo', 'visudo') if binary not in absent)
                    code, output, _, _ = self.execute_guest(
                        program, sudoers,
                        {'action': 'prepare', 'runner_name': 'runway-lease-123', 'allow_guest_sudo': True},
                        present=present, results=results, root_owned=owned)
                    # Preparation is refused, so no bootstrap follows it on this guest.
                    self.assertEqual(code, 1)
                    self.assertEqual(json.loads(output), {'state': 'unknown'})

    def test_opt_in_never_configures_a_non_prepare_action(self):
        for action, expected in (('status', 0), ('collect', 0), ('stop', 0), ('bootstrap', 1)):
            with self.subTest(action=action):
                with tempfile.TemporaryDirectory() as directory:
                    program, sudoers, owned = self.guest_paths(directory)
                    code, _, recorded, ownership = self.execute_guest(
                        program, sudoers,
                        {'action': action, 'runner_name': 'runway-lease-123', 'allow_guest_sudo': True},
                        root_owned=owned)
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
                    program, sudoers, owned = self.guest_paths(directory)
                    results = {SERVICE_REPORT: (0, report)} if report else {}
                    if registered == 'marker':
                        (Path(directory) / 'bootstrap.json').write_text(
                            json.dumps({'runner_name': 'runway-lease-123'}))
                    else:
                        (Path(directory) / 'actions-runner/.runner').write_text('{}')
                    code, output, recorded, ownership = self.execute_guest(
                        program, sudoers,
                        {'action': 'prepare', 'runner_name': 'runway-lease-123', 'allow_guest_sudo': True},
                        results=results, root_owned=owned)
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
                    program, sudoers, owned = self.guest_paths(directory)
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
                        {'action': 'prepare', 'runner_name': 'runway-lease-123', 'allow_guest_sudo': True},
                        root_owned=owned)
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
            program, sudoers, owned = self.guest_paths(directory)
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
                plant=plant, root_owned=owned)
            # O_EXCL|O_NOFOLLOW closes the window between the check and the create, so
            # a drop-in that appears there is never written through or trusted.
            self.assertEqual(code, 1)
            self.assertEqual(json.loads(output), {'state': 'unknown'})
            self.assertEqual(target.read_text(), 'root ALL=(ALL) NOPASSWD: ALL\n')
            self.assertEqual([argv for argv in recorded if any('sudo' in part for part in argv)], [])

    # The opt-in RAM package cache: one named tmpfs over the guest's own cache directory.

    MOUNT_TOOLS = ('mount', 'findmnt', 'umount')
    # What the kernel reports back for the mount this program asks for: the type, the
    # source name, the granted size and the options, exactly as findmnt prints them.
    CACHE_OPTIONS = ('rw,nosuid,nodev,relatime,size=2048m,mode=755,uid=1001,gid=1001,inode64')

    @staticmethod
    def cache_path(directory):
        return Path(directory) / 'cache'

    @staticmethod
    def probe(cache):
        """The exact probe this program issues for one path's mount table."""
        return ('/usr/bin/findmnt', '--bytes', '--noheadings', '--output',
                'SOURCE,FSTYPE,TARGET,SIZE,OPTIONS', '--target', str(cache))

    def findmnt(self, directory, cache=None):
        return self.probe(self.cache_path(directory) if cache is None else cache)

    @staticmethod
    def enclosing():
        """One probe answer: the path is not a mountpoint, so this is its filesystem."""
        return (0, '/dev/vda1 ext4 / 207929917440 rw,relatime\n')

    def mounted(self, directory, source='runway-cache', fstype='tmpfs', size_mib=2048,
                options=CACHE_OPTIONS, cache=None):
        """One probe answer: this source and type, at this size and these options."""
        return (0, '%s %s %s %d %s\n' % (source, fstype,
                                         cache or self.cache_path(directory),
                                         size_mib * 1048576, options))

    def cache_guest(self, directory, mib=2048, results=None, meminfo=None, present=None,
                    cache=None):
        """Prepare a fresh guest with the RAM cache opt-in and the guest's own answers."""
        program, sudoers, owned = self.guest_paths(directory)
        if cache is not None:
            # Re-point this program at a differently shaped cache path, e.g. one whose
            # parent is not the real home directory.
            program = program.replace(str(self.cache_path(directory)), str(cache))
        payload = {'action': 'prepare', 'runner_name': 'runway-lease-123',
                   'cache_tmpfs_mib': mib}
        return (self.execute_guest(program, sudoers, payload, root_owned=owned,
                                   present=self.MOUNT_TOOLS if present is None else present,
                                   results=results, meminfo=meminfo)
                + (program, sudoers, owned))

    @staticmethod
    def mount_calls(recorded, binary='mount'):
        """The calls this program made to one binary of the mount table's own toolkit."""
        return [argv for argv in recorded if argv[0] == '/usr/bin/' + binary]

    def test_cache_opt_in_is_explicit_and_carried_in_the_payload(self):
        for flag, expected in (([], None), (['--cache-tmpfs-mib', '2048'], 2048)):
            with self.subTest(flag=flag):
                with (patch.object(transport.sys, 'argv', self.arguments('prepare') + flag),
                      patch.object(transport.sys, 'stdout', io.StringIO()),
                      patch.object(transport.subprocess, 'run') as run):
                    run.return_value = subprocess.CompletedProcess([], 0, '{"state":"installed"}')
                    self.assertEqual(transport.main(), 0)
                # Absent by default: an unflagged preparation carries no size at all, so no
                # guest can be given a RAM-backed cache an operator never asked for.
                payload = json.loads(run.call_args.kwargs['input'])
                self.assertEqual(payload.get('cache_tmpfs_mib'), expected)

    def test_cache_size_outside_the_supported_range_is_rejected_before_ssh(self):
        for size in ('0', '127', '16385', '-2048'):
            with self.subTest(size=size):
                with (patch.object(transport.sys, 'argv',
                                   self.arguments('prepare') + ['--cache-tmpfs-mib', size]),
                      patch.object(transport.sys, 'stderr', io.StringIO()),
                      patch.object(transport.subprocess, 'run') as run,
                      self.assertRaises(SystemExit)):
                    transport.main()
                run.assert_not_called()
        for size in ('128', '2048', '16384'):
            with self.subTest(size=size):
                with (patch.object(transport.sys, 'argv',
                                   self.arguments('prepare') + ['--cache-tmpfs-mib', size]),
                      patch.object(transport.sys, 'stdout', io.StringIO()),
                      patch.object(transport.subprocess, 'run') as run):
                    run.return_value = subprocess.CompletedProcess([], 0, '{"state":"installed"}')
                    self.assertEqual(transport.main(), 0)
                payload = json.loads(run.call_args.kwargs['input'])
                self.assertEqual(payload['cache_tmpfs_mib'], int(size))

    def test_prepare_without_the_cache_opt_in_touches_no_mount_at_all(self):
        with tempfile.TemporaryDirectory() as directory:
            program, sudoers, owned = self.guest_paths(directory)
            code, output, recorded, ownership = self.execute_guest(
                program, sudoers, {'action': 'prepare', 'runner_name': 'runway-lease-123'},
                root_owned=owned)
            self.assertEqual(code, 0)
            self.assertEqual(json.loads(output), {'state': 'installed'})
            for binary in self.MOUNT_TOOLS:
                self.assertEqual(self.mount_calls(recorded, binary), [])
            # Not even the directory: the default guest is left exactly as it was sealed.
            self.assertFalse(self.cache_path(directory).exists())
            self.assertEqual(ownership, [])

    def test_opt_in_mounts_a_named_bounded_tmpfs_over_the_cache(self):
        with tempfile.TemporaryDirectory() as directory:
            answers = {self.findmnt(directory): [self.enclosing(), self.mounted(directory)]}
            code, output, recorded, ownership, _, _, _ = self.cache_guest(
                directory, results=answers)
            self.assertEqual(code, 0)
            self.assertEqual(json.loads(output), {'state': 'installed'})
            # The sealed image arrives without this directory, so preparation makes one for
            # the account the job runs as: open() applied a umask, so both are set exactly.
            cache = self.cache_path(directory)
            self.assertTrue(cache.is_dir())
            self.assertEqual(stat.S_IMODE(cache.stat().st_mode), 0o755)
            self.assertEqual(ownership, [(1001, 1001)])
            mount, = self.mount_calls(recorded)
            self.assertEqual(mount[:4], ['/usr/bin/mount', '-n', '-t', 'tmpfs'])
            # Bounded, owned by the job's account, and executable: noexec would break the
            # native modules that are built and run out of this very directory.
            self.assertEqual(mount[mount.index('-o') + 1],
                             'rw,nosuid,nodev,size=2048m,uid=1001,gid=1001,mode=0755')
            self.assertNotIn('noexec', ' '.join(mount))
            # The mount carries a source name, so the next preparation can tell its own
            # filesystem from a foreign one instead of stacking a second mount over it.
            self.assertEqual(mount[-2:], ['runway-cache', str(cache)])
            # The path is read before the mount and confirmed after it, never assumed.
            probes = [index for index, argv in enumerate(recorded)
                      if argv[0] == '/usr/bin/findmnt']
            self.assertEqual(len(probes), 2)
            self.assertLess(probes[0], recorded.index(mount))
            self.assertLess(recorded.index(mount), probes[1])
            self.assertEqual(self.mount_calls(recorded, 'umount'), [])
            # Nothing is installed or downloaded into it: the mount only decides where the
            # guest's own downloads will land.
            self.assertEqual(list(cache.iterdir()), [])

    def test_opt_in_mounts_over_an_empty_cache_the_image_already_shipped(self):
        with tempfile.TemporaryDirectory() as directory:
            cache = self.cache_path(directory)
            cache.mkdir()
            answers = {self.findmnt(directory): [self.enclosing(), self.mounted(directory)]}
            code, _, recorded, ownership, _, _, _ = self.cache_guest(
                directory, results=answers)
            self.assertEqual(code, 0)
            # An empty directory the image already carries is mounted as it stands: no
            # second one is created and nothing is chowned behind an operator's back.
            self.assertEqual(ownership, [])
            self.assertEqual(len(self.mount_calls(recorded)), 1)

    def test_cache_refuses_a_directory_it_does_not_own_the_shape_of(self):
        for kind in ('symlink', 'nonempty', 'file'):
            with self.subTest(kind=kind):
                with tempfile.TemporaryDirectory() as directory:
                    cache, planted = self.cache_path(directory), Path(directory) / 'planted'
                    if kind == 'symlink':
                        planted.mkdir()
                        cache.symlink_to(planted)
                    elif kind == 'nonempty':
                        cache.mkdir()
                        (cache / 'yarn').mkdir()
                    else:
                        cache.write_text('')
                    code, output, recorded, _, _, _, _ = self.cache_guest(
                        directory, results={self.findmnt(directory): [self.enclosing()]})
                    # A cache this code does not own the shape of is refused outright, so no
                    # bootstrap follows this preparation on the guest.
                    self.assertEqual(code, 1)
                    self.assertEqual(json.loads(output), {'state': 'unknown'})
                    self.assertEqual(self.mount_calls(recorded), [])
                    self.assertEqual(self.mount_calls(recorded, 'umount'), [])
                    if kind == 'symlink':
                        # lstat never followed it, so the planted directory is left alone.
                        self.assertTrue(cache.is_symlink())
                        self.assertEqual(list(planted.iterdir()), [])

    def test_cache_is_refused_when_the_home_directory_is_not_a_real_one(self):
        for kind in ('symlinked', 'file', 'missing'):
            with self.subTest(kind=kind):
                with tempfile.TemporaryDirectory() as directory:
                    home, planted = Path(directory) / 'home', Path(directory) / 'planted'
                    planted.mkdir()
                    if kind == 'symlinked':
                        home.symlink_to(planted)
                    elif kind == 'file':
                        home.write_text('')
                    else:
                        home.mkdir()
                        home.rmdir()
                    cache = home / '.cache'
                    code, output, recorded, _, _, _, _ = self.cache_guest(
                        directory, cache=cache,
                        results={self.findmnt(directory, cache): [self.enclosing()]})
                    self.assertEqual(code, 1)
                    self.assertEqual(json.loads(output), {'state': 'unknown'})
                    self.assertEqual(self.mount_calls(recorded), [])
                    # The parent is read with lstat, so nothing is created through it.
                    self.assertFalse((planted / '.cache').exists())

    def test_cache_is_refused_unless_the_memory_reserve_is_spare(self):
        # 2048 MiB of cache plus the 2048 MiB this guest keeps in reserve, which is the
        # margin the running database and the job itself are entitled to.
        cases = [('no-estimate', 'MemTotal:       6168576 kB\n', 1, 0),
                 ('unparsable', 'MemAvailable:   many kB\n', 1, 0),
                 ('short-by-one', 'MemAvailable:   %d kB\n' % (4095 * 1024), 1, 0),
                 ('exactly-the-reserve', 'MemAvailable:   %d kB\n' % (4096 * 1024), 0, 1)]
        for name, report, expected, mounts in cases:
            with self.subTest(case=name):
                with tempfile.TemporaryDirectory() as directory:
                    answers = {self.findmnt(directory): [self.enclosing(), self.mounted(directory)]}
                    code, output, recorded, _, _, _, _ = self.cache_guest(
                        directory, results=answers, meminfo=report)
                    self.assertEqual(code, expected)
                    self.assertEqual(json.loads(output)['state'],
                                     'installed' if expected == 0 else 'unknown')
                    # A cache that cannot fit under the reserve would be an out-of-memory
                    # kill mid-job, so it is refused while it is still only a request.
                    self.assertEqual(len(self.mount_calls(recorded)), mounts)
                    if mounts == 0:
                        self.assertFalse(self.cache_path(directory).exists())

    def test_a_cache_that_does_not_come_up_is_never_reported_as_one(self):
        for kind in ('mount-refused', 'absent-after', 'wrong-size-after', 'noexec-after',
                     'no-util-linux'):
            with self.subTest(kind=kind):
                with tempfile.TemporaryDirectory() as directory:
                    results, present = {}, self.MOUNT_TOOLS
                    if kind == 'mount-refused':
                        results[('/usr/bin/mount', '-n', '-t', 'tmpfs', '-o',
                                 'rw,nosuid,nodev,size=2048m,uid=1001,gid=1001,mode=0755',
                                 'runway-cache', str(self.cache_path(directory)))] = (1, '')
                        results[self.findmnt(directory)] = [self.enclosing()]
                    elif kind == 'absent-after':
                        results[self.findmnt(directory)] = [self.enclosing(), self.enclosing()]
                    elif kind == 'wrong-size-after':
                        results[self.findmnt(directory)] = [self.enclosing(),
                                                            self.mounted(directory, size_mib=1024)]
                    elif kind == 'noexec-after':
                        results[self.findmnt(directory)] = [self.enclosing(), self.mounted(
                            directory, options='rw,nosuid,nodev,noexec,relatime,size=2048m')]
                    else:
                        present = ()
                    code, output, recorded, _, _, _, _ = self.cache_guest(
                        directory, results=results, present=present)
                    self.assertEqual(code, 1)
                    self.assertEqual(json.loads(output), {'state': 'unknown'})
                    if kind == 'no-util-linux':
                        # An image without the mount table's own binaries fails closed
                        # instead of preparing a guest whose cache was never mounted.
                        self.assertEqual(self.mount_calls(recorded, 'findmnt'), [])
                    elif kind == 'mount-refused':
                        # Nothing was mounted, so nothing is taken back off.
                        self.assertEqual(self.mount_calls(recorded, 'umount'), [])
                    else:
                        # A filesystem this code could not verify is one the job would still
                        # use, so it comes back off instead of behind a reported success.
                        self.assertEqual(len(self.mount_calls(recorded, 'umount')), 1)
                        self.assertEqual(self.mount_calls(recorded, 'umount')[0][-1],
                                         str(self.cache_path(directory)))

    def test_repeated_prepare_verifies_its_own_mount_without_stacking_another(self):
        with tempfile.TemporaryDirectory() as directory:
            first, _, first_calls, _, _, _, _ = self.cache_guest(
                directory, results={self.findmnt(directory):
                                    [self.enclosing(), self.mounted(directory)]})
            self.assertEqual(first, 0)
            self.assertEqual(len(self.mount_calls(first_calls)), 1)
            # A controller can die before it records the phase, so the same guest is
            # prepared again: the type, size and options the kernel actually gave this
            # mount are re-read, and nothing is mounted over the first one.
            second, output, second_calls, _, _, _, _ = self.cache_guest(
                directory, results={self.findmnt(directory): [self.mounted(directory)]})
            self.assertEqual(second, 0)
            self.assertEqual(json.loads(output), {'state': 'installed'})
            self.assertEqual(self.mount_calls(second_calls), [])
            self.assertEqual(self.mount_calls(second_calls, 'umount'), [])
            self.assertEqual(len(self.mount_calls(second_calls, 'findmnt')), 1)

    def test_a_mount_that_is_not_the_one_asked_for_is_neither_remounted_nor_removed(self):
        # Source, type and size identify this code's own mount; the options identify the
        # filesystem the job would actually get, ownership included. Anything else here is
        # somebody else's, and a wrong owner would fail later as uid 1001, mid-job.
        cases = [('guest-root', {'source': '/dev/vda1'}, None),
                 ('bind-of-home', {'source': '/dev/vda1[/home/runner]'}, None),
                 ('same-name-other-type', {'fstype': 'ext4'}, None),
                 ('half-asked-for', {'size_mib': 1024}, None),
                 ('twice-asked-for', {'size_mib': 4096}, None),
                 ('noexec', {}, 'rw,nosuid,nodev,noexec,relatime,size=2048m,'
                                 'mode=755,uid=1001,gid=1001,inode64'),
                 ('read-only', {}, 'ro,nosuid,nodev,relatime,size=2048m,'
                                   'mode=755,uid=1001,gid=1001,inode64'),
                 ('without-nosuid', {}, 'rw,nodev,relatime,size=2048m,'
                                        'mode=755,uid=1001,gid=1001,inode64'),
                 ('without-nodev', {}, 'rw,nosuid,relatime,size=2048m,'
                                       'mode=755,uid=1001,gid=1001,inode64'),
                 ('root-owned', {}, 'rw,nosuid,nodev,relatime,size=2048m,'
                                    'mode=755,uid=0,gid=1001,inode64'),
                 ('wrong-group', {}, 'rw,nosuid,nodev,relatime,size=2048m,'
                                     'mode=755,uid=1001,gid=0,inode64'),
                 ('without-uid', {}, 'rw,nosuid,nodev,relatime,size=2048m,'
                                     'mode=755,gid=1001,inode64'),
                 ('group-writable', {}, 'rw,nosuid,nodev,relatime,size=2048m,'
                                        'mode=775,uid=1001,gid=1001,inode64')]
        for name, arguments, options in cases:
            with self.subTest(case=name):
                with tempfile.TemporaryDirectory() as directory:
                    reported = self.mounted(directory, options=options or self.CACHE_OPTIONS,
                                            **arguments)
                    code, output, recorded, _, _, _, _ = self.cache_guest(
                        directory, results={self.findmnt(directory): [reported]})
                    self.assertEqual(code, 1)
                    self.assertEqual(json.loads(output), {'state': 'unknown'})
                    # A filesystem this code did not create is never mounted over and never
                    # taken away, whatever it is mounted from, sized at or restricted to.
                    self.assertEqual(self.mount_calls(recorded), [])
                    self.assertEqual(self.mount_calls(recorded, 'umount'), [])

    def test_the_guest_refuses_a_size_the_command_line_would_never_send(self):
        # Zero and an absent key both mean "not requested", so neither is a size here.
        for mib in (127, 16385, 1 << 20):
            with self.subTest(mib=mib):
                with tempfile.TemporaryDirectory() as directory:
                    code, output, recorded, _, _, _, _ = self.cache_guest(
                        directory, mib=mib,
                        results={self.findmnt(directory): [self.enclosing()]})
                    # The command line bounds this already, but the guest is the code that
                    # sizes a tmpfs, so it refuses a size outside the same range itself.
                    self.assertEqual(code, 1)
                    self.assertEqual(json.loads(output), {'state': 'unknown'})
                    self.assertEqual(self.mount_calls(recorded), [])
                    self.assertFalse(self.cache_path(directory).exists())

    def test_cache_never_reaches_a_registered_guest(self):
        for report, registered, expected in [(RUNNING_REPORT, 'marker', 'running'),
                                             (COMPLETED_REPORT, 'marker', 'completed'),
                                             (None, 'dot-runner', 'unknown')]:
            with self.subTest(registered=registered, expected=expected):
                with tempfile.TemporaryDirectory() as directory:
                    program, sudoers, owned = self.guest_paths(directory)
                    results = {SERVICE_REPORT: (0, report)} if report else {}
                    if registered == 'marker':
                        (Path(directory) / 'bootstrap.json').write_text(
                            json.dumps({'runner_name': 'runway-lease-123'}))
                    else:
                        (Path(directory) / 'actions-runner/.runner').write_text('{}')
                    code, output, recorded, _ = self.execute_guest(
                        program, sudoers, {'action': 'prepare', 'runner_name': 'runway-lease-123',
                                           'cache_tmpfs_mib': 2048},
                        present=self.MOUNT_TOOLS, results=results, root_owned=owned)
                    self.assertEqual(code, 1)
                    self.assertEqual(json.loads(output), {'state': expected})
                    # The freshness guard runs first: a lease that already started, or a
                    # clone another lease ran, is only read and never touched.
                    for binary in self.MOUNT_TOOLS:
                        self.assertEqual(self.mount_calls(recorded, binary), [])

    def test_cache_never_configures_a_non_prepare_action(self):
        for action, expected in (('status', 0), ('collect', 0), ('stop', 0), ('bootstrap', 1)):
            with self.subTest(action=action):
                with tempfile.TemporaryDirectory() as directory:
                    program, sudoers, owned = self.guest_paths(directory)
                    code, _, recorded, _ = self.execute_guest(
                        program, sudoers, {'action': action, 'runner_name': 'runway-lease-123',
                                           'cache_tmpfs_mib': 2048},
                        present=self.MOUNT_TOOLS, root_owned=owned)
                    self.assertEqual(code, expected)
                    for binary in self.MOUNT_TOOLS:
                        self.assertEqual(self.mount_calls(recorded, binary), [])

    def test_the_cache_opt_in_leaves_the_namespace_transport_untouched(self):
        with tempfile.TemporaryDirectory() as directory:
            request = self.host_request(directory, port=None)
            request['cache_tmpfs_mib'] = 2048
            code, _, recorded = self.execute_host(request)
            argv, = self.guests(recorded)
            # No mirror, so no reverse forward: the client stays in the Metal root
            # namespace and enters the guest's namespace to make the connection.
            self.assertEqual(argv[:5], ['ip', 'netns', 'exec', 'metal-vm-123', 'ssh'])
            self.assertNotIn('-R', argv)
            self.assertNotIn('ProxyCommand', ' '.join(argv))
            self.assertNotIn('runway-forward', ' '.join(argv))


if __name__ == '__main__':
    unittest.main()
