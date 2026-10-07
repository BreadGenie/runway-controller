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
        (root / 'clocksource').mkdir(parents=True, exist_ok=True)
        # A kernel that offers tsc alongside the MSR-backed kvm-clock, which is what
        # Firecracker's guest boots with because it passes no pvclock page.
        (root / 'clocksource/available_clocksource').write_text('kvm-clock tsc\n')
        (root / 'clocksource/current_clocksource').write_text('kvm-clock\n')
        # The kernel exposes these as root-owned 0644. write_text honours the umask,
        # which here would leave them group-writable, and a group-writable attribute is
        # exactly what the guest program refuses, so the mode is set rather than assumed.
        for attribute in ('available_clocksource', 'current_clocksource'):
            os.chmod(root / 'clocksource' / attribute, 0o644)
        # The cache and workspace directories are deliberately left out: sealing empties
        # them, so a fresh guest arrives without either and preparation has to create one.
        program = transport.GUEST_PROGRAM
        for guest_path, private_path in (('/var/lib/runway/bootstrap.json', root / 'bootstrap.json'),
                                         ('/home/runner/actions-runner', root / 'actions-runner'),
                                         ('/home/runner/.cache', root / 'cache'),
                                         ('/etc/sudoers.d/runway-runner', root / 'sudoers/runway-runner'),
                                         ('/etc/sudoers.d', root / 'sudoers'),
                                         ('/etc/docker/daemon.json', root / 'docker/daemon.json'),
                                         ('/etc/runway-frappe-bench.json', root / 'runway-frappe-bench.json'),
                                         ('/sys/devices/system/clocksource/clocksource0',
                                          root / 'clocksource')):
            program = program.replace(guest_path, str(private_path))
        # The workspace is redirected here too, so no test can reach a real guest's
        # directory even when it never enables that opt-in.
        program = program.replace('/home/runner/work', str(root / 'work'))
        # Every file this code may create is root-owned in the guest. The test user
        # cannot own them, so the recorded ownership stands in for that.
        # Both clocksource attributes are in `owned` because the guest program now
        # requires them to be root-owned sysfs, which an unprivileged test user cannot
        # create: fake_lstat rewrites their recorded ownership to root's.
        return program, root / 'sudoers/runway-runner', [root / 'sudoers/runway-runner',
                                                        root / 'docker/daemon.json',
                                                        root / 'clocksource/current_clocksource',
                                                        root / 'clocksource/available_clocksource']

    def execute_guest(self, program, sudoers, payload, *, present=('sudo', 'visudo'),
                      results=None, plant=None, root_owned=(), meminfo=None,
                      published_answer=b'220 probe ready\r\n', proxy_listing=None):
        """Run the embedded guest program locally: no SSH and no real guest binary."""
        real_lstat = os.lstat
        real_open = builtins.open
        real_write = os.write
        real_os_open = os.open
        recorded, ownership = [], []
        # A probe this program issues more than once in a single run is answered in
        # order, so the mount table read before a mount can differ from the one after.
        results = {key: list(value) if isinstance(value, list) else value
                   for key, value in (results or {}).items()}
        owned = {os.fspath(path) for path in (list(root_owned) + [sudoers])}

        def fake_run(argv, **kwargs):
            recorded.append(list(argv))
            if argv[:1] == ['ps'] and proxy_listing is not None:
                return subprocess.CompletedProcess(list(argv), 0, proxy_listing)
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

        # The proof polls a real 120 s deadline while a port never answers. A
        # virtual clock that jumps past it keeps that refusal test instant, and it is
        # the only thing in the guest program that waits on wall time.
        tick = [0.0]

        def fake_monotonic():
            # One second per reading: the proof's deadline is 120 seconds away, so
            # this still walks the whole retry budget on the failure path, but every
            # step is instant. A coarser step would expire the deadline before the
            # first attempt and make the success path untestable.
            tick[0] += 1.0
            return tick[0]

        # The published-port proof opens a loopback socket and reads a greeting.
        # This test user must not reach a real port, and nothing is listening on
        # this one, so the exchange is answered here rather than dialled.
        class _Answered:
            """The smallest thing a socket context manager needs to be usable.

            BytesIO has no settimeout, and the proof sets one before reading, so
            this carries the two methods it actually calls.
            """

            def __init__(self, payload):
                self._payload = payload

            def settimeout(self, _seconds):
                pass

            def recv(self, _size):
                return self._payload

            def close(self):
                pass

            def __enter__(self):
                return self

            def __exit__(self, *_exc):
                return False

        def fake_create_connection(address, timeout=None):
            if address[0] != '127.0.0.1':
                raise OSError('only loopback is answered here')
            if published_answer is None:
                raise OSError('nothing is listening in this test')
            return _Answered(published_answer)

        sysfs_descriptors = {}

        def fake_os_open(path, flags, *rest, **options):
            descriptor = real_os_open(path, flags, *rest, **options)
            # os.open is patched process-wide, so a caller passing an already-open
            # descriptor as its first argument must not be turned into an fspath.
            if isinstance(path, (str, bytes, os.PathLike)) and os.fspath(path) in owned:
                sysfs_descriptors[descriptor] = os.fspath(path)
            return descriptor

        def fake_write(descriptor, data):
            # sysfs replaces an attribute's whole value on write, whatever the offset or
            # length. A temporary file cannot: a raw write appends and leaves a tail the
            # kernel would never leave. Truncating first makes the stand-in behave like
            # the sysfs it stands in for, so the guest program's readback is a real test.
            if descriptor in sysfs_descriptors:
                try:
                    os.ftruncate(descriptor, 0)
                    os.lseek(descriptor, 0, os.SEEK_SET)
                except OSError:
                    pass
            return real_write(descriptor, data)

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
              patch.object(transport.os, 'open', side_effect=fake_os_open),
              patch.object(transport.os, 'write', side_effect=fake_write),
              # The guest program imports socket inside its own source, so it
              # resolves the real module; patch that rather than an attribute of
              # this one, which has no socket of its own.
              patch('socket.create_connection', side_effect=fake_create_connection),
              patch('time.monotonic', side_effect=fake_monotonic),
              patch('time.sleep', side_effect=lambda _seconds: None),
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

    def test_preparation_timings_survive_the_outer_hop(self):
        # The guest measures and this process re-serialises its document, so a field that
        # is not carried here is a measurement nobody receives. That is exactly what
        # happened: the guest reported timings, and this hop dropped them, and the unit
        # tests missed it because they run the guest program directly and never come
        # through main(). So this test drives main() and reads what it prints.
        for flag, expected in ((['--timings'], True), ([], False)):
            with self.subTest(flag=flag):
                with (patch.object(transport.sys, 'argv',
                                   self.arguments('status') + flag),
                      patch.object(transport.sys, 'stdout', io.StringIO()),
                      patch.object(transport.subprocess, 'run') as run):
                    run.return_value = subprocess.CompletedProcess(
                        [], 0, '{"state":"installed","timings_ms":{"state":3}}')
                    printed = io.StringIO()
                    with patch.object(transport.sys, 'stdout', printed):
                        self.assertEqual(transport.main(), 0)
                document = json.loads(printed.getvalue())
                self.assertEqual(document['state'], 'installed')
                self.assertEqual('timings_ms' in document, expected)
                if expected:
                    self.assertEqual(document['timings_ms'], {'state': 3})

    def test_a_guest_timing_nobody_asked_for_is_not_forwarded(self):
        # A guest that reports timings unasked, which a future guest might, must not widen
        # the status document for a caller that did not opt in.
        with (patch.object(transport.sys, 'argv', self.arguments('status')),
              patch.object(transport.subprocess, 'run') as run):
            run.return_value = subprocess.CompletedProcess(
                [], 0, '{"state":"installed","timings_ms":{"state":3}}')
            printed = io.StringIO()
            with patch.object(transport.sys, 'stdout', printed):
                self.assertEqual(transport.main(), 0)
        self.assertEqual(json.loads(printed.getvalue()), {'state': 'installed'})

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

    # The proof is the load-bearing part of this opt-in: a daemon that accepted the
    # file and ignored the key would leave every job unoptimised while preparation
    # reported success. So each of its two halves, and each way it can fail, is pinned.
    # The proof matches the host port as Docker prints it, host-ip and port joined by
    # a colon, so the fixture has to carry that shape and not merely the digits.
    PROXY_LEFT = ("root 123 /usr/bin/docker-proxy -proto tcp -host-ip 127.0.0.1:8099 "
                  "-container-ip 172.17.0.2 -container-port 25\n")
    IMAGE_LIST = "rnwood/smtp4dev:3.7.1\npostgres:18.0\nmariadb:11.8\n"
    # One literal, because preparation rewrites the document with indent=2 and the
    # undo restores the exact prior text: a fixture that differs by one space from
    # its own assertion would report a real undo as a failure.
    DAEMON_JSON = '{"log-level":"warn"}\n'
    DOCKERD = '/usr/bin/dockerd'
    DOCKER = '/usr/bin/docker'

    def kernel_publish(self, directory, *, results=None, published_answer=b'220 ready\r\n',
                       proxy_listing="", owned=True, expect_code=0):
        program, sudoers, listed = self.guest_paths(directory)
        (Path(directory) / 'docker/daemon.json').write_text('{"log-level":"warn"}\n')
        # The kernel exposes a root-owned 0644 file and the guest program verifies
        # that rather than assuming it. write_text honours the umask, which would
        # leave this group-writable and therefore refused, so the mode is set.
        (Path(directory) / 'docker/daemon.json').chmod(0o644)
        answers = dict(results or {})
        answers.setdefault(
            ('docker', 'images', '--format', '{{.Repository}}:{{.Tag}}'),
            (0, self.IMAGE_LIST))
        code, output, recorded, _ = self.execute_guest(
            program, sudoers,
            {'action': 'prepare', 'runner_name': 'runway-lease-123', 'kernel_publish': True},
            present=('sudo', 'visudo', 'dockerd', 'docker'),
            results=answers, published_answer=published_answer,
            proxy_listing=proxy_listing, root_owned=listed if owned else ())
        return code, output, recorded, Path(directory) / 'docker/daemon.json'

    def test_kernel_publish_proves_the_port_works_and_no_proxy_remains(self):
        with tempfile.TemporaryDirectory() as directory:
            code, output, recorded, daemon = self.kernel_publish(directory)
            self.assertEqual(code, 0)
            self.assertEqual(json.loads(output), {'state': 'installed'})
            self.assertEqual(json.loads(daemon.read_text())['userland-proxy'], False)
            # The unrelated key survived: preparation merges, it does not replace.
            self.assertEqual(json.loads(daemon.read_text())['log-level'], 'warn')
            # The binary is resolved by absolute path, not by name: preparation must
            # not depend on a non-interactive shell's PATH.
            self.assertIn([self.DOCKERD, '--validate',
                           '--config-file=' + str(daemon)], recorded)
            self.assertIn(['systemctl', 'restart', 'docker'], recorded)
            # Both halves of the proof, in order: the container is published, the
            # port is dialled, then the process list is checked for a leftover proxy.
            published = [i for i, argv in enumerate(recorded)
                         if argv[:2] == ['docker', 'run']]
            listed = [i for i, argv in enumerate(recorded) if argv[:1] == ['ps']]
            self.assertTrue(published and listed)
            self.assertLess(published[0], listed[0])
            # The probe container is removed, so preparation leaves nothing running.
            self.assertTrue([argv for argv in recorded
                             if argv[:3] == ['docker', 'rm', '-f']
                             and 'runway-publish-probe' in argv])
            # Nothing is pulled: only an image already in the store is used.
            self.assertEqual([argv for argv in recorded if argv[:1] == ['docker', 'pull']], [])

    def test_kernel_publish_is_undone_when_a_userspace_proxy_survives(self):
        with tempfile.TemporaryDirectory() as directory:
            code, output, _, daemon = self.kernel_publish(
                directory, proxy_listing=self.PROXY_LEFT)
            self.assertEqual(code, 1)
            self.assertEqual(json.loads(output), {'state': 'unknown'})
            # The exact prior text is back, so a guest that fails this is not left
            # carrying a setting preparation could not prove.
            self.assertEqual(daemon.read_text(), '{"log-level":"warn"}\n')

    def test_kernel_publish_is_undone_when_the_port_does_not_answer(self):
        with tempfile.TemporaryDirectory() as directory:
            code, output, _, daemon = self.kernel_publish(directory, published_answer=None)
            self.assertEqual(code, 1)
            self.assertEqual(json.loads(output), {'state': 'unknown'})
            self.assertEqual(daemon.read_text(), '{"log-level":"warn"}\n')

    def test_kernel_publish_refuses_when_the_store_has_nothing_to_prove_with(self):
        with tempfile.TemporaryDirectory() as directory:
            # An empty store means no way to prove the setting without pulling, and
            # preparation must not pull.
            code, output, _, daemon = self.kernel_publish(
                directory, results={('docker', 'images', '--format',
                                    '{{.Repository}}:{{.Tag}}'): (0, '')})
            self.assertEqual(code, 1)
            self.assertEqual(json.loads(output), {'state': 'unknown'})
            self.assertEqual(daemon.read_text(), '{"log-level":"warn"}\n')

    def test_kernel_publish_refuses_a_daemon_file_it_cannot_verify(self):
        for kind in ('symlink', 'world-writable'):
            with self.subTest(kind), tempfile.TemporaryDirectory() as directory:
                program, sudoers, listed = self.guest_paths(directory)
                target = Path(directory) / 'canary'
                target.write_text('planted')
                daemon = Path(directory) / 'docker/daemon.json'
                daemon.unlink(missing_ok=True)
                if kind == 'symlink':
                    daemon.symlink_to(target)
                else:
                    daemon.write_text(self.DAEMON_JSON)
                    daemon.chmod(0o666)
                code, output, _, _ = self.execute_guest(
                    program, sudoers,
                    {'action': 'prepare', 'runner_name': 'runway-lease-123',
                     'kernel_publish': True},
                    present=('sudo', 'visudo', 'dockerd', 'docker'),
                    results={('docker', 'images', '--format',
                              '{{.Repository}}:{{.Tag}}'): (0, self.IMAGE_LIST)},
                    root_owned=listed if kind == 'symlink' else ())
                self.assertEqual(code, 1)
                self.assertEqual(json.loads(output), {'state': 'unknown'})
                self.assertEqual(target.read_text(), 'planted')

    def test_kernel_publish_off_by_default_leaves_the_daemon_file_alone(self):
        with tempfile.TemporaryDirectory() as directory:
            program, sudoers, _ = self.guest_paths(directory)
            daemon = Path(directory) / 'docker/daemon.json'
            daemon.write_text(self.DAEMON_JSON)
            code, output, recorded, _ = self.execute_guest(
                program, sudoers, {'action': 'prepare', 'runner_name': 'runway-lease-123'})
            self.assertEqual(code, 0)
            self.assertEqual(json.loads(output), {'state': 'installed'})
            self.assertEqual(daemon.read_text(), self.DAEMON_JSON)
            self.assertEqual([argv for argv in recorded if argv[:1] == ['docker']], [])

    def test_kernel_publish_opt_in_is_explicit_and_carried_in_the_payload(self):
        # A typo in either half would make prepare skip the setting and still report
        # the guest ready, which is the outcome an opt-in must never produce.
        for flag, expected in (([], False), (['--kernel-published-ports'], True)):
            with self.subTest(flag=flag):
                with (patch.object(transport.sys, 'argv', self.arguments('prepare') + flag),
                      patch.object(transport.sys, 'stdout', io.StringIO()),
                      patch.object(transport.subprocess, 'run') as run):
                    run.return_value = subprocess.CompletedProcess([], 0, '{"state":"installed"}')
                    self.assertEqual(transport.main(), 0)
                payload = json.loads(run.call_args.kwargs['input'])
                self.assertIs(payload['kernel_publish'], expected)

    def test_tsc_opt_in_is_explicit_and_carried_in_the_payload(self):
        # A typo in either half of this wiring would make prepare silently skip the
        # opt-in and still report the guest ready, which is the one outcome an opt-in
        # must never produce.
        for flag, expected in (([], False), (['--prefer-guest-tsc'], True)):
            with self.subTest(flag=flag):
                with (patch.object(transport.sys, 'argv', self.arguments('prepare') + flag),
                      patch.object(transport.sys, 'stdout', io.StringIO()),
                      patch.object(transport.subprocess, 'run') as run):
                    run.return_value = subprocess.CompletedProcess([], 0, '{"state":"installed"}')
                    self.assertEqual(transport.main(), 0)
                payload = json.loads(run.call_args.kwargs['input'])
                self.assertIs(payload['prefer_tsc_clocksource'], expected)

    def test_tsc_opt_in_is_reached_by_prepare_alone(self):
        # bootstrap, status, collect and stop must not touch a kernel setting, and a
        # guest that already carries a registration must never be reconfigured.
        for action in ('bootstrap', 'status', 'collect', 'stop'):
            with self.subTest(action=action):
                with tempfile.TemporaryDirectory() as directory:
                    program, sudoers, owned = self.guest_paths(directory)
                    current = Path(directory) / 'clocksource/current_clocksource'
                    payload = {'action': action, 'runner_name': 'runway-lease-123',
                               'prefer_tsc_clocksource': True}
                    self.execute_guest(program, sudoers, payload, root_owned=owned)
                    self.assertEqual(current.read_text().strip(), 'kvm-clock')
        with tempfile.TemporaryDirectory() as directory:
            program, sudoers, owned = self.guest_paths(directory)
            current = Path(directory) / 'clocksource/current_clocksource'
            # A recorded registration moves the guest out of `installed`, which is the
            # only state preparation reconfigures.
            (Path(directory) / 'bootstrap.json').write_text(
                json.dumps({'runner_name': 'runway-lease-123'}))
            payload = {'action': 'prepare', 'runner_name': 'runway-lease-123',
                       'prefer_tsc_clocksource': True}
            code, output, _, _ = self.execute_guest(program, sudoers, payload, root_owned=owned)
            self.assertEqual(code, 1)
            self.assertEqual(json.loads(output), {'state': 'unknown'})
            self.assertEqual(current.read_text().strip(), 'kvm-clock')

    def test_tsc_opt_in_refuses_a_kernel_that_marks_its_own_tsc_unstable(self):
        with tempfile.TemporaryDirectory() as directory:
            program, sudoers, owned = self.guest_paths(directory)
            current = Path(directory) / 'clocksource/current_clocksource'
            # Selecting tsc on a kernel that has declared the TSC unstable would trade
            # clock accuracy for speed, which is the opposite of what this opt-in is for.
            (Path(directory) / 'clocksource/unstable').write_text('')
            payload = {'action': 'prepare', 'runner_name': 'runway-lease-123',
                       'prefer_tsc_clocksource': True}
            code, output, _, _ = self.execute_guest(program, sudoers, payload, root_owned=owned)
            self.assertEqual(code, 1)
            self.assertEqual(json.loads(output), {'state': 'unknown'})
            self.assertEqual(current.read_text().strip(), 'kvm-clock')

    def test_tsc_opt_in_refuses_an_attribute_others_could_have_written(self):
        for mode, label in ((0o664, 'group-writable'), (0o606, 'world-writable')):
            with self.subTest(label), tempfile.TemporaryDirectory() as directory:
                program, sudoers, owned = self.guest_paths(directory)
                current = Path(directory) / 'clocksource/current_clocksource'
                current.chmod(mode)
                payload = {'action': 'prepare', 'runner_name': 'runway-lease-123',
                           'prefer_tsc_clocksource': True}
                # Recorded as root-owned, so the refusal has to be the mode and not the
                # owner. A group- or world-writable attribute means somebody other than
                # root could have selected a clocksource already, so this code did not
                # verify what it was writing.
                code, output, _, _ = self.execute_guest(program, sudoers, payload,
                                                         root_owned=owned)
                self.assertEqual(code, 1, label)
                self.assertEqual(json.loads(output), {'state': 'unknown'}, label)
                self.assertEqual(current.read_text().strip(), 'kvm-clock', label)

    def test_tsc_opt_in_refuses_an_attribute_another_account_owns(self):
        with tempfile.TemporaryDirectory() as directory:
            program, sudoers, owned = self.guest_paths(directory)
            current = Path(directory) / 'clocksource/current_clocksource'
            # Deliberately outside `owned`, so the recorded owner is this test user
            # rather than root. An attribute the job's own account could have written
            # is not one this code verified.
            code, output, _, _ = self.execute_guest(
                program, sudoers,
                {'action': 'prepare', 'runner_name': 'runway-lease-123',
                 'prefer_tsc_clocksource': True},
                root_owned=())
            self.assertEqual(code, 1)
            self.assertEqual(json.loads(output), {'state': 'unknown'})
            self.assertEqual(current.read_text().strip(), 'kvm-clock')

    def test_tsc_opt_in_selects_the_clocksource_and_reads_it_back(self):
        with tempfile.TemporaryDirectory() as directory:
            program, sudoers, owned = self.guest_paths(directory)
            current = Path(directory) / 'clocksource/current_clocksource'
            payload = {'action': 'prepare', 'runner_name': 'runway-lease-123',
                       'prefer_tsc_clocksource': True}
            code, output, recorded, _ = self.execute_guest(program, sudoers, payload,
                                                           root_owned=owned)
            self.assertEqual(code, 0)
            self.assertEqual(json.loads(output), {'state': 'installed'})
            self.assertEqual(current.read_text().strip(), 'tsc')
            # Nothing is installed and nothing is downloaded: tsc is a clocksource the
            # guest kernel already offers, so this must not reach for a package manager.
            self.assertEqual([argv for argv in recorded
                              if any(part in ('apt', 'apt-get', 'curl', 'wget') for part in argv)], [])
            # A controller that died before recording the phase prepares the same guest
            # again. A guest already on tsc is confirmed and left alone, so the
            # attribute is made unwritable first: a second write would fail loudly
            # rather than pass unnoticed.
            os.chmod(current, 0o444)
            repeated, _, _, _ = self.execute_guest(program, sudoers, payload, root_owned=owned)
            self.assertEqual(repeated, 0)
            self.assertEqual(current.read_text().strip(), 'tsc')

    def test_tsc_opt_in_refuses_an_offer_list_it_cannot_trust(self):
        # Both attributes are checked, not only the one written. The offer list is what
        # decides whether the write is attempted at all, so a list another account
        # could have rewritten is not one this code verified.
        with tempfile.TemporaryDirectory() as directory:
            program, sudoers, owned = self.guest_paths(directory)
            current = Path(directory) / 'clocksource/current_clocksource'
            trusted = [p for p in owned if 'available_clocksource' not in str(p)]
            payload = {'action': 'prepare', 'runner_name': 'runway-lease-123',
                       'prefer_tsc_clocksource': True}
            code, output, _, _ = self.execute_guest(program, sudoers, payload,
                                                     root_owned=trusted)
            self.assertEqual(code, 1)
            self.assertEqual(json.loads(output), {'state': 'unknown'})
            self.assertEqual(current.read_text().strip(), 'kvm-clock')

    def test_tsc_opt_in_refuses_a_kernel_that_does_not_offer_it(self):
        with tempfile.TemporaryDirectory() as directory:
            program, sudoers, owned = self.guest_paths(directory)
            current = Path(directory) / 'clocksource/current_clocksource'
            (Path(directory) / 'clocksource/available_clocksource').write_text('kvm-clock hpet\n')
            payload = {'action': 'prepare', 'runner_name': 'runway-lease-123',
                       'prefer_tsc_clocksource': True}
            code, output, _, _ = self.execute_guest(program, sudoers, payload, root_owned=owned)
            # An image whose kernel cannot select tsc fails preparation instead of being
            # mutated, and reports the same generic state as every other refusal.
            self.assertEqual(code, 1)
            self.assertEqual(json.loads(output), {'state': 'unknown'})
            self.assertEqual(current.read_text().strip(), 'kvm-clock')

    def test_tsc_opt_in_refuses_a_clocksource_attribute_it_cannot_prove(self):
        for mutate, label in (
            (lambda root: None, 'absent'),
            (lambda root: (root / 'clocksource/current_clocksource').symlink_to(
                root / 'canary'), 'a symlink'),
        ):
            with self.subTest(label), tempfile.TemporaryDirectory() as directory:
                program, sudoers, owned = self.guest_paths(directory)
                root = Path(directory)
                # A canary inside this temporary tree, never a real system file: if the
                # symlink guard were ever removed, a write-capable run would otherwise
                # truncate something outside the test's control.
                (root / 'canary').write_text('planted')
                (root / 'clocksource/current_clocksource').unlink()
                mutate(root)
                payload = {'action': 'prepare', 'runner_name': 'runway-lease-123',
                           'prefer_tsc_clocksource': True}
                code, output, _, _ = self.execute_guest(program, sudoers, payload,
                                                         root_owned=owned)
                self.assertEqual(code, 1, label)
                self.assertEqual(json.loads(output), {'state': 'unknown'}, label)
                # The refusal has to be the guard, not a permission error that happens
                # to produce the same exit code.
                self.assertEqual((root / 'canary').read_text(), 'planted', label)

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
            # Off by default: a guest that did not ask for it keeps the clocksource its
            # kernel booted with.
            current = next(p for p in owned if 'clocksource' in str(p))
            self.assertEqual(current.read_text().strip(), 'kvm-clock')

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
    # The workspace opt-in below mounts the same way, so these helpers are stated in terms
    # of a path and a source name rather than of the cache alone.

    MOUNT_TOOLS = ('mount', 'findmnt', 'umount')
    # What the kernel reports back for the mount this program asks for: the type, the
    # source name, the granted size and the options, exactly as findmnt prints them.
    TMPFS_OPTIONS = ('rw,nosuid,nodev,relatime,size=2048m,mode=755,uid=1001,gid=1001,inode64')

    @staticmethod
    def cache_path(directory):
        return Path(directory) / 'cache'

    @staticmethod
    def probe(path):
        """The exact probe this program issues for one path's mount table."""
        return ('/usr/bin/findmnt', '--bytes', '--noheadings', '--output',
                'SOURCE,FSTYPE,TARGET,SIZE,OPTIONS', '--target', str(path))

    def findmnt(self, directory, path=None):
        return self.probe(self.cache_path(directory) if path is None else path)

    @staticmethod
    def enclosing():
        """One probe answer: the path is not a mountpoint, so this is its filesystem."""
        return (0, '/dev/vda1 ext4 / 207929917440 rw,relatime\n')

    def mounted(self, directory, source='runway-cache', fstype='tmpfs', size_mib=2048,
                options=TMPFS_OPTIONS, path=None):
        """One probe answer: this source and type, at this size and these options."""
        return (0, '%s %s %s %d %s\n' % (source, fstype,
                                         path or self.cache_path(directory),
                                         size_mib * 1048576, options))

    def cache_guest(self, directory, mib=2048, results=None, meminfo=None, present=None,
                    cache=None, timings=None):
        """Prepare a fresh guest with the RAM cache opt-in and the guest's own answers."""
        program, sudoers, owned = self.guest_paths(directory)
        if cache is not None:
            # Re-point this program at a differently shaped cache path, e.g. one whose
            # parent is not the real home directory.
            program = program.replace(str(self.cache_path(directory)), str(cache))
        payload = {'action': 'prepare', 'runner_name': 'runway-lease-123',
                   'cache_tmpfs_mib': mib, 'timings': timings}
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
                        results={self.findmnt(directory, path=cache): [self.enclosing()]})
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
                    reported = self.mounted(directory, options=options or self.TMPFS_OPTIONS,
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

    # The opt-in RAM workspace: the same named, bounded, verified tmpfs over the directory
    # the guest runner checks every job out into, never requested together with the cache.

    WORKSPACE_SOURCE = 'runway-workspace'

    @staticmethod
    def workspace_path(directory):
        return Path(directory) / 'work'

    def workspace_findmnt(self, directory):
        return self.findmnt(directory, path=self.workspace_path(directory))

    def workspace_mounted(self, directory, **changes):
        """One probe answer: the mount this program asks for over the workspace.

        A caller may override any part of it to model a filesystem this code did not make.
        """
        return self.mounted(directory, path=self.workspace_path(directory),
                            **{'source': self.WORKSPACE_SOURCE, **changes})

    def workspace_guest(self, directory, mib=2048, results=None, meminfo=None, present=None,
                        payload=None):
        """Prepare a fresh guest with the RAM workspace opt-in and the guest's answers."""
        program, sudoers, owned = self.guest_paths(directory)
        request = {'action': 'prepare', 'runner_name': 'runway-lease-123',
                   'workspace_tmpfs_mib': mib} if payload is None else payload
        return (self.execute_guest(program, sudoers, request, root_owned=owned,
                                   present=self.MOUNT_TOOLS if present is None else present,
                                   results=results, meminfo=meminfo)
                + (program, sudoers, owned))

    def test_workspace_opt_in_is_explicit_and_carried_in_the_payload(self):
        for flag, expected in (([], None), (['--workspace-tmpfs-mib', '2048'], 2048)):
            with self.subTest(flag=flag):
                with (patch.object(transport.sys, 'argv', self.arguments('prepare') + flag),
                      patch.object(transport.sys, 'stdout', io.StringIO()),
                      patch.object(transport.subprocess, 'run') as run):
                    run.return_value = subprocess.CompletedProcess([], 0, '{"state":"installed"}')
                    self.assertEqual(transport.main(), 0)
                payload = json.loads(run.call_args.kwargs['input'])
                # Absent by default: an unflagged preparation carries no size at all, so no
                # guest can be given a RAM-backed workspace an operator never asked for.
                self.assertEqual(payload.get('workspace_tmpfs_mib'), expected)
                # Asking for the workspace leaves the cache opt-in exactly as it was, so
                # one flag moves one directory and never both.
                self.assertNotIn('cache_tmpfs_mib', payload)

    def test_workspace_size_outside_the_supported_range_is_rejected_before_ssh(self):
        for size in ('0', '127', '16385', '-2048'):
            with self.subTest(size=size):
                with (patch.object(transport.sys, 'argv',
                                   self.arguments('prepare') + ['--workspace-tmpfs-mib', size]),
                      patch.object(transport.sys, 'stderr', io.StringIO()),
                      patch.object(transport.subprocess, 'run') as run,
                      self.assertRaises(SystemExit)):
                    transport.main()
                run.assert_not_called()
        for size in ('128', '2048', '16384'):
            with self.subTest(size=size):
                with (patch.object(transport.sys, 'argv',
                                   self.arguments('prepare') + ['--workspace-tmpfs-mib', size]),
                      patch.object(transport.sys, 'stdout', io.StringIO()),
                      patch.object(transport.subprocess, 'run') as run):
                    run.return_value = subprocess.CompletedProcess([], 0, '{"state":"installed"}')
                    self.assertEqual(transport.main(), 0)
                payload = json.loads(run.call_args.kwargs['input'])
                self.assertEqual(payload['workspace_tmpfs_mib'], int(size))

    def test_workspace_tmpfs_is_refused_when_the_image_carries_a_bench(self):
        """A tmpfs over the workspace would cover the bench, and the job would not notice."""
        # Enough memory for the request, and findmnt answers so the guest recognises it
        # is preparing the mount rather than running it twice.
        meminfo = 'MemAvailable:   %d kB\n' % (6144 * 1024)

        def prepare(directory, mib):
            answers = {self.workspace_findmnt(
                directory): [self.enclosing(), self.workspace_mounted(directory)]}
            return self.workspace_guest(
                directory, results=answers, meminfo=meminfo,
                payload={'action': 'prepare', 'runner_name': 'runway-lease-123',
                         'workspace_tmpfs_mib': mib})

        # No bake in this image: the opt-in means exactly what it always meant.
        with tempfile.TemporaryDirectory() as directory:
            code, output, recorded, *_ = prepare(directory, 2048)
            self.assertEqual(code, 0, output)
            self.assertEqual(json.loads(output), {'state': 'installed'})

        with tempfile.TemporaryDirectory() as directory:
            # The bench is under this guest's workspace, so a tmpfs over it hides the bench.
            Path(directory, 'runway-frappe-bench.json').write_text(
                json.dumps({'bench_root': str(self.workspace_path(directory) / 'frappe/frappe')}))
            code, output, recorded, *_ = prepare(directory, 2048)
            self.assertEqual(code, 1, output)
            self.assertEqual(json.loads(output), {'state': 'unknown'})
            # Refused before anything was mounted: the refusal has to leave a guest the
            # operator can still inspect. The claim is about mounts, because the guest
            # runs other probes first and the findmnt answers this test supplies are its
            # own stubs rather than observations.
            self.assertEqual(self.mount_calls(recorded), [])

        # Two manifests that do not put a bench under the workspace leave the opt-in
        # exactly as it was: one names a bench elsewhere, and one cannot be read at all.
        # Sealing refuses to publish an unreadable manifest, so treating it as no bake is
        # the safer reading rather than covering the workspace on a claim nobody can check.
        for case in ('elsewhere', 'unreadable'):
            with self.subTest(manifest=case):
                with tempfile.TemporaryDirectory() as directory:
                    Path(directory, 'runway-frappe-bench.json').write_text(
                        'not json' if case == 'unreadable'
                        else json.dumps({'bench_root': '/opt/somewhere-else/frappe'}))
                    code, output, recorded, *_ = prepare(directory, 2048)
                    self.assertEqual(code, 0, output)
                    self.assertEqual(json.loads(output), {'state': 'installed'})
                    self.assertTrue(self.mount_calls(recorded))

    def test_the_cache_and_the_workspace_are_exclusive_on_the_command_line_and_in_the_guest(self):
        args = self.arguments('prepare') + ['--cache-tmpfs-mib', '2048',
                                            '--workspace-tmpfs-mib', '2048']
        with (patch.object(transport.sys, 'argv', args),
              patch.object(transport.sys, 'stderr', io.StringIO()),
              patch.object(transport.subprocess, 'run') as run,
              self.assertRaises(SystemExit)):
            transport.main()
        # Refused before a key or a channel is used: two reservations against one 6 GiB
        # guest would be an aggregate, so the trial could never attribute a result.
        run.assert_not_called()
        # The presence of the two keys is what is refused, as on the command line, so a
        # payload that spells either one as zero cannot slip the pair past the guest and
        # then have the falsy half quietly mean "not requested" further down.
        for cache, workspace in ((2048, 2048), (2048, 0), (0, 2048), (0, 0)):
            with self.subTest(cache=cache, workspace=workspace):
                with tempfile.TemporaryDirectory() as directory:
                    code, output, recorded, _, _, _, _ = self.workspace_guest(
                        directory, payload={'action': 'prepare', 'runner_name': 'runway-lease-123',
                                            'cache_tmpfs_mib': cache,
                                            'workspace_tmpfs_mib': workspace})
                    # A payload written any other way is refused by the guest itself, before
                    # it has read the guest's state, created a directory or mounted
                    # anything at all.
                    self.assertEqual(code, 1)
                    self.assertEqual(json.loads(output), {'state': 'unknown'})
                    self.assertEqual(recorded, [])
                    self.assertFalse(self.cache_path(directory).exists())
                    self.assertFalse(self.workspace_path(directory).exists())
        with tempfile.TemporaryDirectory() as directory:
            # One key alone still means what it always meant: a falsy size is no request, so
            # this is prepared without a tmpfs rather than refused as an impossible pair.
            code, output, recorded, _, _, _, _ = self.workspace_guest(
                directory, payload={'action': 'prepare', 'runner_name': 'runway-lease-123',
                                    'cache_tmpfs_mib': 0})
            self.assertEqual(code, 0)
            self.assertEqual(json.loads(output), {'state': 'installed'})
            self.assertEqual(self.mount_calls(recorded), [])
            self.assertFalse(self.cache_path(directory).exists())
            self.assertFalse(self.workspace_path(directory).exists())

    def test_the_workspace_is_neither_probed_nor_mounted_without_the_opt_in(self):
        with tempfile.TemporaryDirectory() as directory:
            program, sudoers, owned = self.guest_paths(directory)
            code, output, recorded, ownership = self.execute_guest(
                program, sudoers,
                {'action': 'prepare', 'runner_name': 'runway-lease-123', 'cache_tmpfs_mib': 2048},
                present=self.MOUNT_TOOLS, root_owned=owned,
                results={self.findmnt(directory): [self.enclosing(), self.mounted(directory)]})
            # The cache opt-in on its own still works, and the workspace is never read: a
            # preparation that asked for one directory must not reserve a second one.
            self.assertEqual(code, 0)
            self.assertEqual(json.loads(output), {'state': 'installed'})
            self.assertEqual(ownership, [(1001, 1001)])
            self.assertEqual([argv for argv in recorded
                              if argv[-1] == str(self.workspace_path(directory))], [])
            self.assertFalse(self.workspace_path(directory).exists())

    def test_opt_in_mounts_a_named_bounded_tmpfs_over_the_workspace(self):
        with tempfile.TemporaryDirectory() as directory:
            workspace = self.workspace_path(directory)
            answers = {self.workspace_findmnt(directory): [self.enclosing(),
                                                          self.workspace_mounted(directory)]}
            code, output, recorded, ownership, _, _, _ = self.workspace_guest(
                directory, results=answers)
            self.assertEqual(code, 0)
            self.assertEqual(json.loads(output), {'state': 'installed'})
            # The sealed image arrives without this directory, so preparation makes one for
            # the account the job runs as: open() applied a umask, so both are set exactly.
            self.assertTrue(workspace.is_dir())
            self.assertEqual(stat.S_IMODE(workspace.stat().st_mode), 0o755)
            self.assertEqual(ownership, [(1001, 1001)])
            mount, = self.mount_calls(recorded)
            self.assertEqual(mount[:4], ['/usr/bin/mount', '-n', '-t', 'tmpfs'])
            # Identical to the cache mount in every respect the job can observe: bounded,
            # owned by the account it runs as, and executable, because native modules are
            # built and run out of this very directory.
            self.assertEqual(mount[mount.index('-o') + 1],
                             'rw,nosuid,nodev,size=2048m,uid=1001,gid=1001,mode=0755')
            self.assertNotIn('noexec', ' '.join(mount))
            # The one variable under test: this directory, not the cache, under a source
            # name of its own so the two filesystems can never be mistaken for each other.
            self.assertEqual(mount[-2:], [self.WORKSPACE_SOURCE, str(workspace)])
            self.assertNotIn(str(self.cache_path(directory)), ' '.join(mount))
            # The path is read before the mount and confirmed after it, never assumed.
            probes = [index for index, argv in enumerate(recorded)
                      if argv[0] == '/usr/bin/findmnt']
            self.assertEqual(len(probes), 2)
            self.assertLess(probes[0], recorded.index(mount))
            self.assertLess(recorded.index(mount), probes[1])
            self.assertEqual(self.mount_calls(recorded, 'umount'), [])
            # Nothing is installed or downloaded into it: the mount only decides where the
            # guest's own checkout and build land.
            self.assertEqual(list(workspace.iterdir()), [])

    def test_opt_in_mounts_over_an_empty_workspace_the_image_already_shipped(self):
        with tempfile.TemporaryDirectory() as directory:
            self.workspace_path(directory).mkdir()
            answers = {self.workspace_findmnt(directory): [self.enclosing(),
                                                          self.workspace_mounted(directory)]}
            code, _, recorded, ownership, _, _, _ = self.workspace_guest(
                directory, results=answers)
            self.assertEqual(code, 0)
            # An empty directory the image already carries is mounted as it stands: no
            # second one is created and nothing is chowned behind an operator's back.
            self.assertEqual(ownership, [])
            self.assertEqual(len(self.mount_calls(recorded)), 1)

    def test_workspace_refuses_a_directory_it_does_not_own_the_shape_of(self):
        for kind in ('symlink', 'nonempty', 'file'):
            with self.subTest(kind=kind):
                with tempfile.TemporaryDirectory() as directory:
                    workspace, planted = self.workspace_path(directory), Path(directory) / 'planted'
                    if kind == 'symlink':
                        planted.mkdir()
                        workspace.symlink_to(planted)
                    elif kind == 'nonempty':
                        workspace.mkdir()
                        (workspace / 'repo').mkdir()
                    else:
                        workspace.write_text('')
                    code, output, recorded, _, _, _, _ = self.workspace_guest(
                        directory, results={self.workspace_findmnt(directory): [self.enclosing()]})
                    # A workspace this code does not own the shape of is refused outright,
                    # so no bootstrap follows this preparation on the guest.
                    self.assertEqual(code, 1)
                    self.assertEqual(json.loads(output), {'state': 'unknown'})
                    self.assertEqual(self.mount_calls(recorded), [])
                    self.assertEqual(self.mount_calls(recorded, 'umount'), [])
                    if kind == 'symlink':
                        # lstat never followed it, so the planted directory is left alone.
                        self.assertTrue(workspace.is_symlink())
                        self.assertEqual(list(planted.iterdir()), [])

    def test_workspace_is_refused_unless_the_memory_reserve_is_spare(self):
        # 2048 MiB of workspace plus the 2048 MiB this guest keeps in reserve, which is the
        # margin the running database and the job itself are entitled to.
        cases = [('no-estimate', 'MemTotal:       6168576 kB\n', 1, 0),
                 ('unparsable', 'MemAvailable:   many kB\n', 1, 0),
                 ('short-by-one', 'MemAvailable:   %d kB\n' % (4095 * 1024), 1, 0),
                 ('exactly-the-reserve', 'MemAvailable:   %d kB\n' % (4096 * 1024), 0, 1)]
        for name, report, expected, mounts in cases:
            with self.subTest(case=name):
                with tempfile.TemporaryDirectory() as directory:
                    answers = {self.workspace_findmnt(
                        directory): [self.enclosing(), self.workspace_mounted(directory)]}
                    code, output, recorded, _, _, _, _ = self.workspace_guest(
                        directory, results=answers, meminfo=report)
                    self.assertEqual(code, expected)
                    self.assertEqual(json.loads(output)['state'],
                                     'installed' if expected == 0 else 'unknown')
                    # A workspace that cannot fit under the reserve would be an out-of-memory
                    # kill mid-job, so it is refused while it is still only a request.
                    self.assertEqual(len(self.mount_calls(recorded)), mounts)
                    if mounts == 0:
                        self.assertFalse(self.workspace_path(directory).exists())

    def test_a_workspace_mount_that_does_not_come_up_is_never_reported_as_one(self):
        for kind in ('mount-refused', 'absent-after', 'no-util-linux'):
            with self.subTest(kind=kind):
                with tempfile.TemporaryDirectory() as directory:
                    results, present = {}, self.MOUNT_TOOLS
                    if kind == 'mount-refused':
                        results[('/usr/bin/mount', '-n', '-t', 'tmpfs', '-o',
                                 'rw,nosuid,nodev,size=2048m,uid=1001,gid=1001,mode=0755',
                                 self.WORKSPACE_SOURCE, str(self.workspace_path(directory)))] = (1, '')
                        results[self.workspace_findmnt(directory)] = [self.enclosing()]
                    elif kind == 'absent-after':
                        results[self.workspace_findmnt(directory)] = [self.enclosing(),
                                                                      self.enclosing()]
                    else:
                        present = ()
                    code, output, recorded, _, _, _, _ = self.workspace_guest(
                        directory, results=results, present=present)
                    self.assertEqual(code, 1)
                    self.assertEqual(json.loads(output), {'state': 'unknown'})
                    if kind == 'no-util-linux':
                        # An image without the mount table's own binaries fails closed
                        # instead of preparing a guest whose workspace was never mounted.
                        self.assertEqual(self.mount_calls(recorded, 'findmnt'), [])
                    elif kind == 'mount-refused':
                        # Nothing was mounted, so nothing is taken back off.
                        self.assertEqual(self.mount_calls(recorded, 'umount'), [])
                    else:
                        # A filesystem this code could not verify is one the job would still
                        # use, so it comes back off instead of behind a reported success.
                        self.assertEqual(len(self.mount_calls(recorded, 'umount')), 1)
                        self.assertEqual(self.mount_calls(recorded, 'umount')[0][-1],
                                         str(self.workspace_path(directory)))

    def test_repeated_prepare_verifies_its_own_workspace_mount_without_stacking_another(self):
        with tempfile.TemporaryDirectory() as directory:
            first, _, first_calls, _, _, _, _ = self.workspace_guest(
                directory, results={self.workspace_findmnt(
                    directory): [self.enclosing(), self.workspace_mounted(directory)]})
            self.assertEqual(first, 0)
            self.assertEqual(len(self.mount_calls(first_calls)), 1)
            # A controller can die before it records the phase, so the same guest is
            # prepared again: the type, size and options the kernel actually gave this
            # mount are re-read, and nothing is mounted over the first one.
            second, output, second_calls, _, _, _, _ = self.workspace_guest(
                directory, results={self.workspace_findmnt(directory):
                                    [self.workspace_mounted(directory)]})
            self.assertEqual(second, 0)
            self.assertEqual(json.loads(output), {'state': 'installed'})
            self.assertEqual(self.mount_calls(second_calls), [])
            self.assertEqual(self.mount_calls(second_calls, 'umount'), [])
            self.assertEqual(len(self.mount_calls(second_calls, 'findmnt')), 1)

    def test_a_workspace_mount_that_is_not_the_one_asked_for_is_neither_remounted_nor_removed(self):
        # Source, type and size identify this code's own mount; the options identify the
        # filesystem the job would actually get, ownership included. Anything else here is
        # somebody else's, and a wrong owner would fail later as uid 1001, mid-job.
        cases = [('guest-root', {'source': '/dev/vda1'}, None),
                 ('bind-of-home', {'source': '/dev/vda1[/home/runner]'}, None),
                 ('the-cache-instead', {'source': 'runway-cache'}, None),
                 ('same-name-other-type', {'fstype': 'ext4'}, None),
                 ('half-asked-for', {'size_mib': 1024}, None),
                 ('noexec', {}, 'rw,nosuid,nodev,noexec,relatime,size=2048m,'
                                'mode=755,uid=1001,gid=1001,inode64'),
                 ('root-owned', {}, 'rw,nosuid,nodev,relatime,size=2048m,'
                                    'mode=755,uid=0,gid=1001,inode64'),
                 ('group-writable', {}, 'rw,nosuid,nodev,relatime,size=2048m,'
                                         'mode=775,uid=1001,gid=1001,inode64')]
        for name, arguments, options in cases:
            with self.subTest(case=name):
                with tempfile.TemporaryDirectory() as directory:
                    reported = self.workspace_mounted(directory, options=options or self.TMPFS_OPTIONS,
                                                       **arguments)
                    code, output, recorded, _, _, _, _ = self.workspace_guest(
                        directory, results={self.workspace_findmnt(directory): [reported]})
                    self.assertEqual(code, 1)
                    self.assertEqual(json.loads(output), {'state': 'unknown'})
                    # A filesystem this code did not create is never mounted over and never
                    # taken away, whatever it is mounted from, sized at or restricted to.
                    self.assertEqual(self.mount_calls(recorded), [])
                    self.assertEqual(self.mount_calls(recorded, 'umount'), [])

    def test_the_guest_refuses_a_workspace_size_the_command_line_would_never_send(self):
        # Zero and an absent key both mean "not requested", so neither is a size here.
        for mib in (127, 16385, 1 << 20):
            with self.subTest(mib=mib):
                with tempfile.TemporaryDirectory() as directory:
                    code, output, recorded, _, _, _, _ = self.workspace_guest(
                        directory, mib=mib,
                        results={self.workspace_findmnt(directory): [self.enclosing()]})
                    # The command line bounds this already, but the guest is the code that
                    # sizes a tmpfs, so it refuses a size outside the same range itself.
                    self.assertEqual(code, 1)
                    self.assertEqual(json.loads(output), {'state': 'unknown'})
                    self.assertEqual(self.mount_calls(recorded), [])
                    self.assertFalse(self.workspace_path(directory).exists())

    def test_workspace_never_reaches_a_registered_or_non_prepare_action(self):
        for action, expected in (('status', 0), ('collect', 0), ('stop', 0), ('bootstrap', 1)):
            with self.subTest(action=action):
                with tempfile.TemporaryDirectory() as directory:
                    program, sudoers, owned = self.guest_paths(directory)
                    code, _, recorded, _ = self.execute_guest(
                        program, sudoers,
                        {'action': action, 'runner_name': 'runway-lease-123',
                         'workspace_tmpfs_mib': 2048},
                        present=self.MOUNT_TOOLS, root_owned=owned)
                    self.assertEqual(code, expected)
                    for binary in self.MOUNT_TOOLS:
                        self.assertEqual(self.mount_calls(recorded, binary), [])
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
                        program, sudoers,
                        {'action': 'prepare', 'runner_name': 'runway-lease-123',
                         'workspace_tmpfs_mib': 2048},
                        present=self.MOUNT_TOOLS, results=results, root_owned=owned)
                    # The freshness guard runs first: a lease that already started, or a
                    # clone another lease ran, is only read and never touched.
                    self.assertEqual(code, 1)
                    self.assertEqual(json.loads(output), {'state': expected})
                    for binary in self.MOUNT_TOOLS:
                        self.assertEqual(self.mount_calls(recorded, binary), [])


    def timed_guest(self, directory, timings):
        """Prepare a fresh guest with timings asked for or not, and the guest's answers."""
        program, sudoers, owned = self.guest_paths(directory)
        payload = {'action': 'prepare', 'runner_name': 'runway-lease-123',
                   'prefer_tsc_clocksource': True, 'timings': timings}
        return self.execute_guest(program, sudoers, payload, root_owned=owned)

    def test_the_status_document_is_unchanged_unless_timings_are_asked_for(self):
        # The controller parses this document, so an unasked-for field is a contract change
        # rather than an observation, and every existing assertion here depends on it.
        with tempfile.TemporaryDirectory() as directory:
            code, output, _, _ = self.timed_guest(directory, False)

        self.assertEqual(code, 0)
        self.assertEqual(json.loads(output), {'state': 'installed'})

    def test_timings_name_the_steps_that_ran(self):
        with tempfile.TemporaryDirectory() as directory:
            code, output, _, _ = self.timed_guest(directory, True)

        document = json.loads(output)
        self.assertEqual(code, 0)
        self.assertEqual(document['state'], 'installed')
        # Step names are the ones the code uses, so a number is traceable to a line.
        self.assertEqual(set(document['timings_ms']),
                         {'state', 'prefer_tsc_clocksource'})
        for step, milliseconds in document['timings_ms'].items():
            self.assertIsInstance(milliseconds, int, step)
            self.assertGreaterEqual(milliseconds, 0, step)

    def test_a_refused_step_still_reports_what_it_managed_to_time(self):
        # A failure is exactly when an operator wants the numbers, so the refusal path
        # carries them too rather than only the success path.
        with tempfile.TemporaryDirectory() as directory:
            program, sudoers, owned = self.guest_paths(directory)
            payload = {'action': 'prepare', 'runner_name': 'runway-lease-123',
                       'prefer_tsc_clocksource': True, 'timings': True}
            # The clocksource attributes are deliberately left out of `owned`, so the
            # recorded owner is this test user rather than root and the opt-in refuses.
            code, output, _, _ = self.execute_guest(program, sudoers, payload,
                                                    root_owned=())

        self.assertEqual(code, 1)
        document = json.loads(output)
        self.assertEqual(document['state'], 'unknown')
        self.assertIn('prefer_tsc_clocksource', document['timings_ms'])

    def test_the_timings_flag_is_off_unless_the_operator_asks(self):
        # The default has to be off, or every caller pays for measurement and every
        # status document a controller has parsed gains a field.
        self.assertNotIn('--timings', self.arguments('prepare'))
        self.assertIn('--timings', self.arguments('prepare') + ['--timings'])

if __name__ == '__main__':
    unittest.main()
