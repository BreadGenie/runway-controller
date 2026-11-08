#!/usr/bin/env python3
"""Own preparation and one-use bootstrap of a disposable runner guest.

The SSH adapter ships this standalone file unchanged. Tests call execute with a
private filesystem layout; paths supplied over the wire are never accepted.
"""

import base64
import json
import os
import pwd
import re
import ssl
import shutil
import socket
import stat
import subprocess
import sys
import tempfile
import time
from datetime import UTC, datetime
from pathlib import Path
from typing import NamedTuple


class _Paths(NamedTuple):
    """Internal filesystem adapter: baked guest defaults or private test files."""
    system_root: Path = Path('/')
    marker: Path = Path('/var/lib/runway/bootstrap.json')
    runner: Path = Path('/home/runner/actions-runner')
    cache: Path = Path('/home/runner/.cache')
    workspace: Path = Path('/home/runner/work')
    sudoers: Path = Path('/etc/sudoers.d/runway-runner')
    clocksource: Path = Path('/sys/devices/system/clocksource/clocksource0')
    daemon_config: Path = Path('/etc/docker/daemon.json')
    manifest: Path = Path('/etc/runway-frappe-bench.json')
    secret: Path = Path('/home/runner/runway.jit')
    wrapper: Path = Path('/usr/local/bin/runway-guest-runner')


def install_regional_cache(bundle, paths, call):
    """Transactional fresh-clone install; payload contains bytes, never guest paths."""
    root = paths.system_root
    changed = []
    created = []
    unit = 'runway-cache-relay.service'
    active = enabled = False
    started = False
    service_touched = False

    def safe_directory(directory):
        if directory == directory.parent:
            return
        safe_directory(directory.parent)
        try:
            entry = directory.lstat()
        except FileNotFoundError:
            directory.mkdir(mode=0o755)
            created.append(directory)
            return
        if not stat.S_ISDIR(entry.st_mode):
            raise ValueError('unsafe install directory')
        relative = directory.relative_to(root) if directory.is_relative_to(root) else None
        if (relative and relative.parts and relative.parts[0] in ('etc', 'opt')
                and (entry.st_uid != 0 or entry.st_mode & 0o022)):
            raise ValueError('untrusted install directory')

    def replace(path, data, mode):
        safe_directory(path.parent)
        try:
            entry = path.lstat()
            if not stat.S_ISREG(entry.st_mode):
                raise ValueError('unsafe install target')
            previous = (path.read_bytes(), stat.S_IMODE(entry.st_mode), entry.st_uid, entry.st_gid)
        except FileNotFoundError:
            previous = None
        if previous == (data, mode, 0, 0):
            return
        changed.append((path, previous))
        write(path, data, mode, 0, 0)

    def write(path, data, mode, uid, gid):
        fd, temporary = tempfile.mkstemp(dir=path.parent, prefix='.runway-')
        try:
            with os.fdopen(fd, 'wb') as stream:
                os.fchown(stream.fileno(), uid, gid)
                os.fchmod(stream.fileno(), mode)
                stream.write(data)
                stream.flush()
                os.fsync(stream.fileno())
            os.replace(temporary, path)
        finally:
            if os.path.exists(temporary):
                os.unlink(temporary)

    def require(argv):
        if call(argv).returncode:
            raise ValueError('cache service installation failed')

    try:
        if set(bundle) not in ({'service', 'ca', 'allowlist'}, {'service', 'ca', 'allowlist', 'node'}):
            raise ValueError('invalid cache bundle')
        service = bundle['service']
        if not re.fullmatch(r'internal-[a-z0-9][a-z0-9.-]*:[0-9]+', service):
            raise ValueError('invalid service name')
        if not 1 <= int(service.rsplit(':', 1)[1]) <= 65535:
            raise ValueError('invalid service port')
        files = {key: base64.b64decode(bundle[key], validate=True) for key in bundle if key != 'service'}
        if 'node' in files and not files['node'].startswith(b'\x7fELF'):
            raise ValueError('invalid cache helpers')
        context = ssl.SSLContext(ssl.PROTOCOL_TLS_CLIENT)
        context.load_verify_locations(cadata=files['ca'].decode('ascii'))
        active = call(['systemctl', 'is-active', '--quiet', unit]).returncode == 0
        enabled = call(['systemctl', 'is-enabled', '--quiet', unit]).returncode == 0
        # Do not disturb a working relay while validating or staging input.
        nodes = [paths.runner / 'externals' / version / 'bin/node' for version in ('node20', 'node24')]
        for node in nodes:
            if not node.is_file() or node.is_symlink():
                raise ValueError('missing baked cache-node; rebuild runner image or use development override')
            real = node.with_name('node.real')
            if 'node' in files:
                if not real.exists():
                    replace(real, node.read_bytes(), stat.S_IMODE(node.stat().st_mode))
                elif not real.is_file() or real.is_symlink():
                    raise ValueError('unsafe real node')
                replace(node, files['node'], 0o755)
            elif not real.is_file() or real.is_symlink():
                raise ValueError('missing baked cache-node (rebuild runner image)')
        replace(root / 'etc/runway/cache-allowlist.json', files['allowlist'], 0o644)
        for node in nodes:
            entry = node.lstat()
            if entry.st_uid != 0 or entry.st_mode & 0o022:
                raise ValueError('unsafe baked cache-node')
            check = call([str(node), '--runway-cache-check-allowlist', str(root / 'etc/runway/cache-allowlist.json')])
            if check.returncode or check.stdout.strip() != 'cache-node/v1 allowlist/v1':
                raise ValueError('baked cache-node missing or too old for shipped allowlist; rebuild runner image or use development override')
        for helper in ('opt/runway/bin/cache-relay', 'etc/systemd/system/' + unit):
            target = root / helper
            entry = target.lstat()
            if not stat.S_ISREG(entry.st_mode) or entry.st_uid != 0 or entry.st_mode & 0o022:
                raise ValueError('missing or unsafe baked cache relay/unit; rebuild runner image')
        replace(root / 'etc/runway/cache-ca.pem', files['ca'], 0o644)
        config = {'version': 1, 'mode': 'service', 'service': service, 'ca_file': '/etc/runway/cache-ca.pem'}
        replace(root / 'etc/runway/cache.json', (json.dumps(config) + '\n').encode(), 0o644)
        # /run is tmpfs: recreate the runner-owned decision directory each boot.
        replace(root / 'etc/tmpfiles.d/runway-cache.conf', b'd /run/runway 0700 runner runner -\n', 0o644)
        runtime = root / 'run/runway'
        safe_directory(runtime)
        account = pwd.getpwnam('runner')
        if runtime in created:
            descriptor = os.open(runtime, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
            try:
                os.fchown(descriptor, account.pw_uid, account.pw_gid)
                os.fchmod(descriptor, 0o700)
            finally:
                os.close(descriptor)
        else:
            entry = runtime.lstat()
            if entry.st_uid != account.pw_uid or entry.st_gid != account.pw_gid or stat.S_IMODE(entry.st_mode) != 0o700:
                raise ValueError('unsafe decision directory')
        require(['systemd-tmpfiles', '--create', str(root / 'etc/tmpfiles.d/runway-cache.conf')])
        require(['systemctl', 'daemon-reload'])
        service_touched = True
        require(['systemctl', 'enable', unit])
        started = True
        require(['systemctl', 'restart', unit])
        # Type=exec proves execve, not Python's successful bind/config load.
        # Require the local listener before the controller may request a JIT.
        deadline = time.monotonic() + 2
        while True:
            try:
                with socket.create_connection(('127.0.0.1', 18080), timeout=0.2):
                    break
            except OSError:
                require(['systemctl', 'is-active', '--quiet', unit])
                if time.monotonic() >= deadline:
                    raise ValueError('cache relay not listening')
                time.sleep(0.05)
        require(['systemctl', 'is-active', '--quiet', unit])
        return True
    except (OSError, ValueError, TypeError, KeyError, subprocess.SubprocessError) as error:
        print(f'regional cache prepare failed: {error}', file=sys.stderr)
        if started:
            call(['systemctl', 'stop', unit])
        if service_touched and not enabled:
            call(['systemctl', 'disable', unit])
        for path, previous in reversed(changed):
            if previous is None:
                path.unlink(missing_ok=True)
            else:
                write(path, *previous)
        if changed:
            call(['systemctl', 'daemon-reload'])
        if active and (changed or started):
            call(['systemctl', 'start', unit])
        for directory in reversed(created):
            try:
                directory.rmdir()
            except OSError:
                pass
        return False


def execute(payload, *, _paths=None):
    """Apply one request, returning (exit status, public status document).

    Supports prepare, bootstrap, status, collect and stop. Each invocation owns
    its rollback and timing state. JIT material is never included in the result.
    """
    paths = _paths or _Paths()
    response = {}
    if payload.get('action') not in ('prepare', 'bootstrap', 'status', 'collect', 'stop'):
        return 1, {'state': 'unknown'}
    action = payload['action']
    name = payload['runner_name']
    # The two RAM-backed directories each reserve memory against the same guest, so asking for
    # both would be an aggregate reservation rather than one measured variable: on a 6 GiB
    # guest two of them together can exceed what a job can see. The command line refuses the
    # pair before it opens a channel; this refuses a payload written any other way, on the
    # presence of the two keys exactly as the command line does, before anything has been
    # created, mounted or configured.
    if 'cache_tmpfs_mib' in payload and 'workspace_tmpfs_mib' in payload:
        response.update(state='unknown')
        return 1, response or None
    marker = paths.marker
    runner = paths.runner
    cache = paths.cache
    workspace = paths.workspace
    sudoers = paths.sudoers
    sudoers_rule = 'runner ALL=(ALL) NOPASSWD: ALL\n'
    # Firecracker exposes no pvclock page to the guest, so the kernel's kvm-clock is
    # MSR-backed and the vDSO has nothing to read in userspace: every clock_gettime in
    # every process becomes a full syscall. Measured on this host's guest, that costs
    # about 205 ns per read against 57 ns with tsc selected and 49 ns on a GitHub-hosted
    # runner. tsc is offered alongside kvm-clock and reading it is one instruction.
    clocksource = paths.clocksource
    clocksource_current = clocksource / 'current_clocksource'
    clocksource_available = clocksource / 'available_clocksource'
    clocksource_unstable = clocksource / 'unstable'
    preferred_clocksource = 'tsc'
    daemon_config = paths.daemon_config
    # Yarn and uv fill this one directory with about 1.3 GiB of packages during a job and
    # read it back on every restore, so it is mounted under a name a repeated preparation
    # can recognise as its own instead of stacking a second mount over the first.
    cache_source = 'runway-cache'
    # The runner checks every job out into the workspace and builds there. It gets the same
    # treatment as the cache, under a source name of its own: source, type and size together
    # are how a repeated preparation tells its own filesystem from a foreign one.
    workspace_source = 'runway-workspace'
    # The guest already keeps a running database on its own tmpfs, so a directory sized
    # against all the memory a job can see would buy disk latency with an out-of-memory kill.
    tmpfs_reserve_mib = 2048
    # The same bounds the command line enforces for both opt-ins, so the guest refuses a size
    # it was never asked to support even if a future caller sends one.
    tmpfs_min_mib, tmpfs_max_mib = 128, 16384
    # The exact text preparation replaced, so a refused mirror leaves Docker as it was.
    previous = None
    # Absolute lookup: a non-interactive ssh command inherits no operator PATH, and
    # preparation must not depend on one.
    root_path = '/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin'

    def call(argv):
        return subprocess.run(argv, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, text=True, timeout=30)

    timings = {}
    # Off unless an operator asks, so the status document a controller parses stays exactly
    # what it was before anything here was measured. Preparation is the largest part of what a
    # job waits for, and with this the only way to attribute it was to change one setting and
    # run a whole job.
    want_timings = bool(payload.get('timings'))


    def timed(name, function, *arguments):
        """Run one step, recording how long it took in milliseconds when timings are wanted.

        The step name is the one the code already uses, so a number in the report is
        traceable to a line rather than to a counter somewhere else.
        """
        if not want_timings:
            return function(*arguments)
        started = time.monotonic()
        try:
            return function(*arguments)
        finally:
            timings[name] = round((time.monotonic() - started) * 1000)


    def report(state):
        """Build the status, adding step timings only when requested."""
        document = {'state': state}
        if timings:
            document['timings_ms'] = timings
        response.clear()
        response.update(document)


    def state():
        if marker.exists():
            recorded = json.loads(marker.read_text())
            if recorded['runner_name'] != name:
                return 'unknown'
            report = call(['systemctl', 'show', 'runway-runner.service',
                           '--property=LoadState', '--property=ActiveState',
                           '--property=ExecMainStartTimestampMonotonic']).stdout
            properties = dict(line.split('=', 1) for line in report.splitlines() if '=' in line)
            started = int(properties.get('ExecMainStartTimestampMonotonic', '0') or '0')
            if properties.get('LoadState') != 'loaded' or started <= 0:
                return 'unknown'
            return 'running' if properties.get('ActiveState') in ('active', 'activating') else 'completed'
        if (runner / '.runner').exists() or not (runner / 'bin/Runner.Listener').is_file():
            return 'unknown'
        return 'installed' if call(['runuser', '-u', 'runner', '--', 'docker', 'info']).returncode == 0 else 'unknown'
    def grant_runner_sudo():
        """Give a fresh disposable guest's runner account passwordless sudo, opt-in only.

        Nothing is installed or downloaded: the baked image must already carry sudo(8) and
        visudo(8), so an image without them fails preparation instead of being mutated.
        """
        sudo = shutil.which('sudo', path=root_path)
        visudo = shutil.which('visudo', path=root_path)
        if sudo is None or visudo is None:
            return False
        try:
            recorded = os.lstat(sudoers)
        except FileNotFoundError:
            recorded = None
        except OSError:
            return False
        if recorded is None:
            try:
                directory = os.lstat(sudoers.parent)
            except OSError:
                return False
            if not stat.S_ISDIR(directory.st_mode):
                return False
            try:
                # O_EXCL|O_NOFOLLOW creates a fresh regular file: never write through a
                # symlink, never truncate, and never reuse a file another account planted.
                handle = os.open(sudoers, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o440)
            except OSError:
                return False
            with os.fdopen(handle, 'w') as stream:
                # open() applies the umask; set the exact mode and owner sudo(8) requires.
                os.fchmod(stream.fileno(), 0o440)
                os.fchown(stream.fileno(), 0, 0)
                stream.write(sudoers_rule)
                stream.flush()
                os.fsync(stream.fileno())
            try:
                recorded = os.lstat(sudoers)
            except OSError:
                return False
        elif not stat.S_ISREG(recorded.st_mode):
            # A symlink, directory or device is refused rather than read: lstat never
            # followed it here, and sudo(8) would.
            return False
        # sudo(8) skips a drop-in that group or world can write, not one it can read, so the
        # conventional root-owned 0440 mode is correct here; refuse any extra bit and any
        # owner but root. A re-prepared guest re-validates the identical rule instead of
        # rewriting a foreign one.
        if recorded.st_mode & 0o7777 != 0o440 or recorded.st_uid != 0:
            return False
        try:
            if sudoers.read_text() != sudoers_rule:
                return False
        except OSError:
            return False
        if call([visudo, '-c', '-f', str(sudoers)]).returncode != 0:
            return False
        # Prove the effective rule, not just the file: sudo must succeed with no password.
        return call(['runuser', '-u', 'runner', '--', sudo, '-n', 'true']).returncode == 0
    def root_owned_sysfs(path):
        """True when `path` is a root-owned sysfs attribute only root could have changed.

        lstat, never stat: a symlink here would redirect the write below, so it is
        refused rather than followed. The condition mirrors read_daemon rather than
        grant_runner_sudo. sudo(8) skips a drop-in that group or world can write, so
        that check is an exact mode; here the requirement is only that nobody but
        root could have selected a clocksource already, and the kernel exposes these
        as root-owned 0644. Demanding that exact mode would refuse a 0444 or 0640
        attribute that is equally safe.
        """
        try:
            recorded = os.lstat(path)
        except OSError:
            return False
        return (stat.S_ISREG(recorded.st_mode) and recorded.st_uid == 0
                and not recorded.st_mode & 0o022)


    def prefer_tsc_clocksource():
        """Select the guest's tsc clocksource, opt-in only.

        Nothing is installed or downloaded: tsc is a clocksource the guest kernel
        already offers in `available_clocksource`, so an image whose kernel does not
        offer it fails preparation instead of being mutated. A kernel that offers it
        but cannot select it also fails, rather than leaving a guest reporting a
        clocksource the job will not get.

        `unstable` is refused when the kernel exposes it. A root-selected tsc on a
        kernel that has marked its own TSC unstable trades clock accuracy for speed,
        which is the opposite of what this opt-in is for, so it is a refusal rather
        than a judgement call.

        Idempotent, and re-verified on a repeated preparation: a guest already on tsc
        is confirmed and left alone rather than written again.
        """
        if not root_owned_sysfs(clocksource_available) or not root_owned_sysfs(clocksource_current):
            return False
        # Presence only. The kernel creates this attribute to say the TSC is not to be
        # trusted; there is nothing in it to read.
        if clocksource_unstable.exists():
            return False
        try:
            offered = clocksource_available.read_text().split()
            current = clocksource_current.read_text().strip()
        except OSError:
            return False
        if preferred_clocksource not in offered:
            return False
        if current == preferred_clocksource:
            return True
        try:
            # O_NOFOLLOW binds the write to this exact inode, which the lstat above only
            # checked separately. Nothing in the test suite can exercise that difference,
            # because it cannot widen the window between the two calls; it is there so
            # that a symlink cannot be substituted in a guest this code did not build.
            # Neither O_CREAT nor O_TRUNC: the attribute provably exists, and neither
            # means anything for an attribute that takes a whole value on write.
            descriptor = os.open(clocksource_current, os.O_WRONLY | os.O_NOFOLLOW)
        except OSError:
            return False
        try:
            os.write(descriptor, preferred_clocksource.encode())
        except OSError:
            return False
        finally:
            os.close(descriptor)
        try:
            return clocksource_current.read_text().strip() == preferred_clocksource
        except OSError:
            return False
    def read_daemon():
        """Read the guest Docker configuration. Returns (status, document).

        Absent, unreadable and planted are distinguished so preparation can undo only its
        own work. A symlink, a non-regular file, a foreign owner or a group- or
        world-writable file is refused rather than rewritten: this code never follows or
        truncates a file it did not create.
        """
        try:
            entry = os.lstat(daemon_config)
        except FileNotFoundError:
            return 'absent', {}
        except OSError:
            return 'refused', None
        if not stat.S_ISREG(entry.st_mode) or entry.st_uid != 0 or entry.st_mode & 0o022:
            return 'refused', None
        try:
            raw = daemon_config.read_text()
        except OSError:
            return 'refused', None
        try:
            document = json.loads(raw)
        except ValueError:
            return 'refused', None
        if not isinstance(document, dict):
            return 'refused', None
        return 'ours', document
    def replace_daemon(text):
        """Install the daemon configuration atomically, preserving its ownership."""
        try:
            handle, staging = tempfile.mkstemp(dir=str(daemon_config.parent),
                                               prefix='.daemon.json.', suffix='.runway')
        except OSError:
            return False
        try:
            os.fchown(handle, 0, 0)
            os.fchmod(handle, 0o644)
            with os.fdopen(handle, 'w') as stream:
                stream.write(text)
                stream.flush()
                os.fsync(stream.fileno())
            os.replace(staging, str(daemon_config))
        except OSError:
            try:
                os.close(handle)
            except OSError:
                pass
            try:
                os.unlink(staging)
            except OSError:
                pass
            return False
        directory = os.open(str(daemon_config.parent), os.O_RDONLY | os.O_DIRECTORY)
        os.fsync(directory)
        os.close(directory)
        return True
    def undo_daemon(status):
        """Put Docker's configuration back the way preparation found it."""
        if status == 'ours':
            replace_daemon(previous)
        elif status == 'absent':
            try:
                os.unlink(str(daemon_config))
            except OSError:
                pass
        call(['systemctl', 'restart', 'docker'])
    def probe_image():
        """Pick an image already in the guest's store to prove a published port works.

        Preparation must not pull: a warm store would make the next pull free for
        reasons unrelated to what is being configured. So this reads what the image
        already carries, preferring the service images the baked runner image ships.
        """
        images = call(['docker', 'images', '--format', '{{.Repository}}:{{.Tag}}']).stdout
        available = [line.strip() for line in images.splitlines() if line.strip()]
        for preferred in ('rnwood/smtp4dev:3.7.1', 'postgres:18.0', 'mariadb:11.8'):
            if preferred in available:
                return preferred
        return available[0] if available else None


    def published_port_works(port, image):
        """Prove a loopback published port answers and that no userspace proxy was left.

        Both halves matter. The port answering shows the setting did not break
        publishing; the absence of a `docker-proxy` process shows it took effect at
        all. A setting the daemon silently ignored would leave the job unoptimised
        while preparation reported success, which is the one outcome an opt-in like
        this must never produce, so it is measured rather than assumed.

        Only an image already in the guest's store is used, so nothing is pulled.
        """
        name = 'runway-publish-probe'
        call(['docker', 'rm', '-f', name])
        # Port 25 is where the preferred image's SMTP service listens; any published
        # port would do, this one is simply known to answer quickly.
        started = call(['docker', 'run', '-d', '--rm', '--name', name,
                        '-p', '127.0.0.1:%d:25' % port, image])
        if started.returncode != 0:
            return False
        # This whole proof runs inside `prepare`, which the controller bounds with its
        # `transport_timeout_seconds`, 90 s in the dogfood configuration. A budget of
        # more than that turns a container that is merely slow to start into a
        # preparation that times out, which the controller reports as a guest failure
        # rather than as this setting. Keep the wait well inside the smaller of the two
        # and treat a port that is not up by then as a refusal.
        deadline = time.monotonic() + 25
        answered = False
        while time.monotonic() < deadline:
            try:
                with socket.create_connection(('127.0.0.1', port), timeout=3) as probe:
                    probe.settimeout(3)
                    answered = bool(probe.recv(64))
                break
            except OSError:
                time.sleep(2)
        call(['docker', 'rm', '-f', name])
        if not answered:
            return False
        # Nothing may be left listening in userspace for this port.
        listing = call(['ps', '-eo', 'args']).stdout
        return not [line for line in listing.splitlines()
                    if 'docker-proxy' in line and (':%d ' % port) in line]


    def prefer_kernel_publish():
        """Publish container ports through the kernel's netfilter DNAT, opt-in only.

        Docker normally reaches a published port through a `docker-proxy` userspace
        process. Measured on this guest, that relay costs 101 to 116 microseconds per
        request on a connection that is already open, against a measurement noise
        floor of about 30 microseconds. The benchmark workflow's 335,599 database
        statements all cross it, which predicts 34 to 39 seconds of a 489 second test
        phase, and a GitHub-hosted runner pays the same hop, so it closes real ground
        rather than moving a baseline.

        Two live jobs on the dogfood pool with this set and the clocksource opt-in
        left off shortened the 708-test phase by 57.92 seconds against five runs that
        set neither, 14.7 sample standard deviations, reproducing to 1.40 seconds,
        both passing all 708 tests. That is larger than the 34 to 39 seconds the relay
        cost predicts, and the microbenchmark's raw capture was not preserved, so
        treat the end-to-end figure as the result and the relay cost only as the
        mechanism. See docs/frappe-performance.md.

        The flag is off by default. With it, `prepare`, and only `prepare`, and only
        while the guest still reports `installed`, sets the key, validates the file
        before the daemon is pointed at it, restarts Docker before it has served a
        container, and then proves both that a published port still answers and that
        no userspace proxy remains. Any failure restores the exact prior text and
        preparation fails closed, because a setting the daemon ignored would leave
        the job unoptimised while reporting success.

        Scope: the guest's own Docker daemon on its own loopback. It changes no Metal
        host value, no network namespace and no credential, and it is applied before
        registration so no job observes a partial change. `bootstrap`, `status`,
        `collect` and `stop` never touch it.

        Limitation, stated rather than hidden: with no userspace proxy, a published
        port relies on the kernel's DNAT, which does not cover every network
        topology, and there is no userspace fallback for UDP. A guest whose ports do
        not come up is refused by the proof above rather than passed to a job.
        """
        nonlocal previous
        daemon = shutil.which('dockerd', path=root_path)
        client = shutil.which('docker', path=root_path)
        if daemon is None or client is None:
            return False
        status, document = read_daemon()
        if document is None:
            return False
        image = probe_image()
        if image is None:
            # Nothing to prove the setting with, and pulling is not this code's job.
            return False
        # Already set, by a previous preparation or by the image: confirm and stop,
        # rather than rewrite an identical document and restart Docker for nothing.
        if document.get('userland-proxy') is False:
            return published_port_works(8099, image)
        previous = '' if status == 'absent' else daemon_config.read_text()
        document['userland-proxy'] = False
        if not replace_daemon(json.dumps(document, indent=2, sort_keys=True) + '\n'):
            return False
        if call([daemon, '--validate', '--config-file=' + str(daemon_config)]).returncode != 0:
            undo_daemon(status)
            return False
        if call(['systemctl', 'restart', 'docker']).returncode != 0:
            undo_daemon(status)
            return False
        if not published_port_works(8099, image):
            undo_daemon(status)
            return False
        return call(['runuser', '-u', 'runner', '--', client, 'info']).returncode == 0


    def enable_mirror(port):
        """Point this fresh guest's Docker at the operator's loopback registry mirror.

        Only an unregistered fresh clone reaches here, and preparation runs before any job,
        so Docker is restarted before it has served a container. Nothing is installed,
        downloaded or authenticated: the mirror is anonymous and read-only, reachable only
        through a reverse SSH forward to this host's loopback. The images are deliberately
        not pre-pulled here, because a warm local store would hide the effect being measured.
        """
        nonlocal previous
        daemon = shutil.which('dockerd', path=root_path)
        client = shutil.which('docker', path=root_path)
        if daemon is None or client is None:
            return False
        status, document = read_daemon()
        if document is None:
            return False
        # registry-mirrors takes a URL; insecure-registry takes a bare host:port. The
        # daemon stores a mirror with its trailing slash, so write back the same form to
        # keep a repeated preparation a no-op.
        for key, addition in (('registry-mirrors', 'http://127.0.0.1:%d/' % port),
                              ('insecure-registries', '127.0.0.1:%d' % port)):
            values = document.get(key, [])
            if not isinstance(values, list) or any(not isinstance(value, str) for value in values):
                return False
            document[key] = values + ([] if addition in values else [addition])
        previous = '' if status == 'absent' else daemon_config.read_text()
        if not replace_daemon(json.dumps(document, indent=2, sort_keys=True) + '\n'):
            return False
        # Validate before the daemon is pointed at the file, so no unknown key and no bad
        # JSON can leave this guest without a working Docker. A rejected file is undone.
        if call([daemon, '--validate', '--config-file=' + str(daemon_config)]).returncode != 0:
            undo_daemon(status)
            return False
        if call(['systemctl', 'restart', 'docker']).returncode != 0:
            undo_daemon(status)
            return False
        reported = call([client, 'info', '--format', '{{json .RegistryConfig.Mirrors}}'])
        mirrors = None
        if reported.returncode == 0:
            try:
                mirrors = json.loads(reported.stdout)
            except ValueError:
                mirrors = None
        # A mirror the daemon does not report is a mirror the job would not use, which is
        # exactly the silent no-op this opt-in exists to prevent. Undo rather than report
        # success, so an operator sees the failure instead of an unoptimised job.
        if not isinstance(mirrors, list) or 'http://127.0.0.1:%d/' % port not in mirrors:
            undo_daemon(status)
            return False
        # The job runs as the runner account, so prove the restarted daemon answers there.
        if call(['runuser', '-u', 'runner', '--', client, 'info']).returncode != 0:
            undo_daemon(status)
            return False
        return True
    def available_mib():
        """Report MemAvailable in MiB: the kernel's own estimate of what a job may claim."""
        try:
            with open('/proc/meminfo') as report:
                for line in report:
                    field, _, rest = line.partition(':')
                    if field == 'MemAvailable':
                        return int(rest.split()[0]) // 1024
        except (OSError, ValueError, IndexError):
            return -1
        return -1
    def tmpfs_state(findmnt, path, source, mib):
        """Report 'ours', 'foreign' or 'unmounted' for the filesystem at one path.

        Source, type, the size the kernel granted and the options all have to match what
        preparation asked for: a filesystem of another type under this name is not one this
        code mounted. The ownership and mode are options too, and the job writes here as
        uid 1001, so a mount that dropped them is refused here rather than failing mid-job. A
        noexec is refused as well, because native modules are built and run out of this
        directory. A path that is not a mountpoint answers with the filesystem containing it,
        whose target is an ancestor, so only a line naming this path counts at all.
        """
        report = call([findmnt, '--bytes', '--noheadings', '--output',
                       'SOURCE,FSTYPE,TARGET,SIZE,OPTIONS', '--target', str(path)])
        if report.returncode != 0:
            return 'unmounted'
        for line in report.stdout.splitlines():
            fields = line.split()
            if len(fields) != 5 or fields[2] != str(path) or not fields[3].isdigit():
                continue
            options = set(fields[4].split(','))
            if (fields[0], fields[1], int(fields[3])) != (source, 'tmpfs', mib * 1048576):
                return 'foreign'
            # The kernel records what it granted normalised, so the mode reads 755 and not
            # 0755, and it adds relatime and inode64 of its own: ask for a subset, not a set.
            wanted = {'rw', 'nosuid', 'nodev', 'uid=1001', 'gid=1001', 'mode=755'}
            return 'ours' if wanted <= options and 'noexec' not in options else 'foreign'
        return 'unmounted'
    def create_guest_directory(path):
        """Create one directory the sealed image leaves out, for the account the job runs as."""
        try:
            # The parent is read with lstat, so a symlinked or non-directory home is refused
            # here. Nothing untrusted has run on this guest yet, and nothing does until the job
            # starts, so this check and the mkdir need no lock between them.
            if not stat.S_ISDIR(os.lstat(path.parent).st_mode):
                return False
            os.mkdir(path, 0o755)
            handle = os.open(str(path), os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
            try:
                # open() applied a umask: the account the job runs as must own this outright.
                os.fchown(handle, 1001, 1001)
                os.fchmod(handle, 0o755)
            finally:
                os.close(handle)
            return True
        except OSError:
            return False
    def baked_bench_root():
        """The bench root this image recorded, or None when it carries no bake.

        Read from the manifest rather than assumed, so an image rebuilt with the bench
        somewhere else is judged by where it actually is.
        """
        manifest = paths.manifest
        if not manifest.is_file():
            return None
        try:
            document = json.loads(manifest.read_text())
        except (OSError, ValueError):
            # An unreadable manifest is treated as no bake. Sealing refuses to publish one in
            # that state, so a guest reaching here was not sealed by this repository, and
            # keeping the workspace as it is the safer reading.
            return None
        root = document.get('bench_root')
        return Path(root) if isinstance(root, str) else None
    def workspace_tmpfs_hides_bench():
        """Whether a tmpfs over the workspace would cover a bench the image carries."""
        bench = baked_bench_root()
        if bench is None:
            return False
        try:
            bench.relative_to(workspace)
        except ValueError:
            return False
        return True
    def tmpfs_filesystem(path, source, mib):
        """Back one fresh guest directory with bounded RAM, on the first preparation.

        The mount carries the caller's source name, so a repeated preparation recognises its
        own filesystem and re-reads the type, size and options the kernel gave it instead of
        mounting over it again. Nothing is installed or downloaded: only where the guest's own
        downloads and builds land.
        """
        mount = shutil.which('mount', path=root_path)
        findmnt = shutil.which('findmnt', path=root_path)
        umount = shutil.which('umount', path=root_path)
        if mount is None or findmnt is None or umount is None:
            return False
        # The command line already bounds this, but the guest is the code that sizes a tmpfs,
        # so the same range holds here rather than depending on there being one caller.
        if not tmpfs_min_mib <= mib <= tmpfs_max_mib:
            return False
        state = tmpfs_state(findmnt, path, source, mib)
        if state != 'unmounted':
            # Its own filesystem, or a foreign one: both are verified where they are, because
            # a second mount over either would only hide the first.
            return state == 'ours'
        absent = False
        try:
            # lstat never followed a symlink, so one planted here is refused rather than
            # mounted through, and only an empty directory counts as unused.
            if not stat.S_ISDIR(os.lstat(path).st_mode) or os.listdir(path):
                return False
        except FileNotFoundError:
            # Sealing empties these directories, so absence is the ordinary state of a fresh
            # guest.
            absent = True
        except OSError:
            return False
        available = available_mib()
        if available < mib + tmpfs_reserve_mib:
            # Refused while it is still only a request: nothing created, nothing mounted.
            return False
        if absent and not create_guest_directory(path):
            return False
        options = 'rw,nosuid,nodev,size=%dm,uid=1001,gid=1001,mode=0755' % mib
        if call([mount, '-n', '-t', 'tmpfs', '-o', options, source, str(path)]).returncode != 0:
            return False
        if tmpfs_state(findmnt, path, source, mib) == 'ours':
            return True
        # A filesystem this code did not verify is one the job would still use, so it comes
        # back off here instead of being left behind a reported success.
        call([umount, str(path)])
        return False
    if action in ('status', 'prepare'):
        current = timed('state', state)
        if action == 'prepare' and current == 'installed':
            # Operator opt-ins, and only while this is an unregistered fresh clone: any
            # resulting root is the disposable Firecracker guest's own, never Metal's.
            if payload.get('allow_guest_sudo') and not timed('grant_runner_sudo', grant_runner_sudo):
                report('unknown')
                return 1, response or None
            # Before the mirror and the two mounts, so that this setting, which is the
            # only one preparation cannot put back, is the one most likely to be left
            # applied: a later refusal cannot undo it. It goes first so that the refusals
            # which can be retried cleanly come after it and report first.
            if payload.get('prefer_tsc_clocksource') and not timed('prefer_tsc_clocksource', prefer_tsc_clocksource):
                report('unknown')
                return 1, response or None
            if payload.get('kernel_publish') and not timed('kernel_publish', prefer_kernel_publish):
                report('unknown')
                return 1, response or None
            if payload.get('mirror_port') and not timed('registry_mirror', enable_mirror, int(payload['mirror_port'])):
                report('unknown')
                return 1, response or None
            # A refusal from here on leaves the three opt-ins above applied, including the
            # clocksource, which this code cannot put back. That is deliberate: this is a
            # disposable guest and preparation is idempotent, so the retry re-verifies them
            # rather than rolling them back.
            if payload.get('cache_tmpfs_mib') and not timed(
                    'cache_tmpfs', tmpfs_filesystem, cache, cache_source,
                    int(payload['cache_tmpfs_mib'])):
                report('unknown')
                return 1, response or None
            if payload.get('workspace_tmpfs_mib'):
                # A tmpfs over /home/runner/work would cover the bench this image carries and
                # the job would find nothing there. The bake's manifest says where it put the
                # bench, so the refusal follows that rather than a path written here twice.
                if workspace_tmpfs_hides_bench():
                    print('This image carries a baked bench under ' + str(workspace)
                          + '; a tmpfs there would hide it. Run without --workspace-tmpfs-mib.',
                          file=sys.stderr)
                    report('unknown')
                    return 1, response or None
                if not timed(
                        'workspace_tmpfs', tmpfs_filesystem, workspace, workspace_source,
                        int(payload['workspace_tmpfs_mib'])):
                    report('unknown')
                    return 1, response or None
            if payload.get('regional_cache') and not timed(
                    'regional_cache', install_regional_cache, payload['regional_cache'], paths, call):
                report('unknown')
                return 1, response or None
        report(current)
        return (0 if action == 'status' or current == 'installed' else 1), response or None
    if action == 'collect':
        report = call(['journalctl', '-u', 'runway-runner.service', '--no-pager', '-n', '200'])
        response.update(state=state(), diagnostics=report.stdout[-131072:])
        return report.returncode, response or None
    if action == 'stop':
        call(['systemctl', 'stop', 'runway-runner.service'])
        response.update(state=state())
        return 0, response or None
    if marker.exists() or (runner / '.runner').exists():
        response.update(state=state())
        return 1, response or None
    if state() != 'installed' or not payload.get('jit'):
        return 1, response or None
    runtime = 1800
    if payload.get('deadline'):
        runtime = min(3600, int((datetime.fromisoformat(payload['deadline']) - datetime.now(UTC)).total_seconds()))
        if runtime < 1:
            return 1, response or None
    marker.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    with marker.open('x') as stream:
        os.chmod(marker, 0o600)
        json.dump({'runner_name': name}, stream)
        stream.flush()
        os.fsync(stream.fileno())
    directory = os.open(marker.parent, os.O_RDONLY | os.O_DIRECTORY)
    os.fsync(directory)
    os.close(directory)
    secret = paths.secret
    fd = os.open(secret, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(fd, 'w') as stream:
        stream.write(payload['jit'] + '\n')
        stream.flush()
        os.fsync(stream.fileno())
    os.chown(secret, 1001, 1001)
    wrapper = paths.wrapper
    wrapper.write_text(payload['wrapper'])
    wrapper.chmod(0o755)
    started = call(['systemd-run', '--unit=runway-runner', '--uid=runner', '--gid=runner',
                    '-p', 'RuntimeMaxSec=' + str(runtime), '-p', 'WorkingDirectory=' + str(runner),
                    str(wrapper), str(runner), str(secret)])
    response.update(state=state())
    return started.returncode, response or None


def main():
    code, document = execute(json.load(sys.stdin))
    if document is not None:
        print(json.dumps(document))
    return code


if __name__ == '__main__':
    sys.exit(main())
