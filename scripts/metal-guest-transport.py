#!/usr/bin/env python3
"""Bootstrap one disposable Metal guest over an operator's SSH channel."""

import argparse
from datetime import UTC, datetime
import json
import os
import re
import shlex
import subprocess
import sys
from pathlib import Path

GUEST_PROGRAM = r'''
import json, os, shutil, socket, stat, subprocess, sys, tempfile, time
from datetime import UTC, datetime
from pathlib import Path
payload = json.load(sys.stdin)
action = payload['action']
name = payload['runner_name']
# The two RAM-backed directories each reserve memory against the same guest, so asking for
# both would be an aggregate reservation rather than one measured variable: on a 6 GiB
# guest two of them together can exceed what a job can see. The command line refuses the
# pair before it opens a channel; this refuses a payload written any other way, on the
# presence of the two keys exactly as the command line does, before anything has been
# created, mounted or configured.
if 'cache_tmpfs_mib' in payload and 'workspace_tmpfs_mib' in payload:
    print(json.dumps({'state': 'unknown'}))
    sys.exit(1)
marker = Path('/var/lib/runway/bootstrap.json')
runner = Path('/home/runner/actions-runner')
cache = Path('/home/runner/.cache')
workspace = Path('/home/runner/work')
sudoers = Path('/etc/sudoers.d/runway-runner')
sudoers_rule = 'runner ALL=(ALL) NOPASSWD: ALL\n'
# Firecracker exposes no pvclock page to the guest, so the kernel's kvm-clock is
# MSR-backed and the vDSO has nothing to read in userspace: every clock_gettime in
# every process becomes a full syscall. Measured on this host's guest, that costs
# about 205 ns per read against 57 ns with tsc selected and 49 ns on a GitHub-hosted
# runner. tsc is offered alongside kvm-clock and reading it is one instruction.
clocksource = Path('/sys/devices/system/clocksource/clocksource0')
clocksource_current = clocksource / 'current_clocksource'
clocksource_available = clocksource / 'available_clocksource'
clocksource_unstable = clocksource / 'unstable'
preferred_clocksource = 'tsc'
daemon_config = Path('/etc/docker/daemon.json')
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
    process. Measured on this guest, that relay costs about 110 microseconds per
    request on a connection that is already open, against a measurement noise
    floor of about 30 microseconds. The benchmark workflow's 335,599 database
    statements all cross it, so this is worth roughly 36 to 48 seconds of a
    489 second test phase, and a GitHub-hosted runner pays the same hop, so it
    closes real ground rather than moving a baseline.

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
    global previous
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
    global previous
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
    current = state()
    if action == 'prepare' and current == 'installed':
        # Operator opt-ins, and only while this is an unregistered fresh clone: any
        # resulting root is the disposable Firecracker guest's own, never Metal's.
        if payload.get('allow_guest_sudo') and not grant_runner_sudo():
            print(json.dumps({'state': 'unknown'}))
            sys.exit(1)
        # Before the mirror and the two mounts, so that this setting, which is the
        # only one preparation cannot put back, is the one most likely to be left
        # applied: a later refusal cannot undo it. It goes first so that the refusals
        # which can be retried cleanly come after it and report first.
        if payload.get('prefer_tsc_clocksource') and not prefer_tsc_clocksource():
            print(json.dumps({'state': 'unknown'}))
            sys.exit(1)
        if payload.get('kernel_publish') and not prefer_kernel_publish():
            print(json.dumps({'state': 'unknown'}))
            sys.exit(1)
        if payload.get('mirror_port') and not enable_mirror(int(payload['mirror_port'])):
            print(json.dumps({'state': 'unknown'}))
            sys.exit(1)
        # A refusal from here on leaves the three opt-ins above applied, including the
        # clocksource, which this code cannot put back. That is deliberate: this is a
        # disposable guest and preparation is idempotent, so the retry re-verifies them
        # rather than rolling them back.
        if payload.get('cache_tmpfs_mib') and not tmpfs_filesystem(
                cache, cache_source, int(payload['cache_tmpfs_mib'])):
            print(json.dumps({'state': 'unknown'}))
            sys.exit(1)
        if payload.get('workspace_tmpfs_mib') and not tmpfs_filesystem(
                workspace, workspace_source, int(payload['workspace_tmpfs_mib'])):
            print(json.dumps({'state': 'unknown'}))
            sys.exit(1)
    print(json.dumps({'state': current}))
    sys.exit(0 if action == 'status' or current == 'installed' else 1)
if action == 'collect':
    report = call(['journalctl', '-u', 'runway-runner.service', '--no-pager', '-n', '200'])
    print(json.dumps({'state': state(), 'diagnostics': report.stdout[-131072:]}))
    sys.exit(report.returncode)
if action == 'stop':
    call(['systemctl', 'stop', 'runway-runner.service'])
    print(json.dumps({'state': state()}))
    sys.exit(0)
if marker.exists() or (runner / '.runner').exists():
    print(json.dumps({'state': state()}))
    sys.exit(1)
if state() != 'installed' or not payload.get('jit'):
    sys.exit(1)
runtime = 1800
if payload.get('deadline'):
    runtime = min(3600, int((datetime.fromisoformat(payload['deadline']) - datetime.now(UTC)).total_seconds()))
    if runtime < 1:
        sys.exit(1)
marker.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
with marker.open('x') as stream:
    os.chmod(marker, 0o600)
    json.dump({'runner_name': name}, stream)
    stream.flush()
    os.fsync(stream.fileno())
directory = os.open(marker.parent, os.O_RDONLY | os.O_DIRECTORY)
os.fsync(directory)
os.close(directory)
secret = Path('/home/runner/runway.jit')
fd = os.open(secret, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
with os.fdopen(fd, 'w') as stream:
    stream.write(payload['jit'] + '\n')
    stream.flush()
    os.fsync(stream.fileno())
os.chown(secret, 1001, 1001)
wrapper = Path('/usr/local/bin/runway-guest-runner')
wrapper.write_text(payload['wrapper'])
wrapper.chmod(0o755)
started = call(['systemd-run', '--unit=runway-runner', '--uid=runner', '--gid=runner',
                '-p', 'RuntimeMaxSec=' + str(runtime), '-p', 'WorkingDirectory=' + str(runner),
                str(wrapper), str(runner), str(secret)])
print(json.dumps({'state': state()}))
sys.exit(started.returncode)
'''

HOST_PROGRAM = r'''
import json, os, re, shlex, shutil, stat, subprocess, sys, time
from pathlib import Path
GUEST_PROBE = """
import json, sys, urllib.request
# An explicit empty proxy keeps the probe on the reverse forward itself rather than on
# any proxy a login profile or system-wide setting might point loopback requests at.
opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
try:
    with opener.open('http://127.0.0.1:%s/v2/' % sys.argv[1], timeout=10) as response:
        print(json.dumps({'registry': response.status == 200}))
except Exception:
    print(json.dumps({'registry': False}))
"""
payload = json.load(sys.stdin)
action = payload['action']
port = payload.get('mirror_port')
namespace = 'metal-' + payload['vm_id']
guest = 'root@172.16.0.2'
# The SSH client stays in Metal's root network namespace and enters the guest's
# namespace only to make the TCP connection. A reverse forward's local end is delivered
# to the client process, so a client running inside the namespace would hand the guest's
# 127.0.0.1 to that namespace's loopback instead of the host's, and the mirror would be
# unreachable while every check reported success. ProxyCommand keeps the client out here.
def options(durable=False):
    common = ['-o', 'BatchMode=yes', '-o', 'ForwardAgent=no', '-o', 'IdentitiesOnly=yes',
              '-o', 'StrictHostKeyChecking=accept-new', '-o', 'ConnectTimeout=10',
              '-o', 'UserKnownHostsFile=' + payload['known_hosts'],
              '-i', payload['key_file']]
    if port:
        common += ['-o', 'ProxyCommand=' + shlex.join(
            ['ip', 'netns', 'exec', namespace, 'socat', '-', 'TCP:172.16.0.2:22'])]
    if durable:
        # A master holds a forward, so it must notice a guest that stopped answering
        # instead of sitting on a dead connection forever. ControlPersist=no only stops
        # it after the connection is already gone; these bounds are what detect that.
        common += ['-o', 'ServerAliveInterval=10', '-o', 'ServerAliveCountMax=3']
    return common
def control(socket, *arguments, timeout=30):
    # ssh(1) requires a destination even for a control verb, and names the host whose
    # control socket it means. Without it this is a usage error, so every check would
    # report no master and every prepare would open a second forward.
    command = ['ssh', '-S', socket, '-o', 'BatchMode=yes', *arguments, guest]
    # The control-master verbs report on stderr. That stream carries a pid and a status
    # line here, never a key, a token or workflow output, so it is read and discarded.
    return subprocess.run(command, stdout=subprocess.DEVNULL, stderr=subprocess.PIPE,
                          text=True, timeout=timeout)
def master_running(socket):
    return control(socket, '-O', 'check').returncode == 0
def reachable():
    """Ask the guest to complete a registry request over its own loopback port.

    This is the only proof that matters: the listener exists only if the guest's sshd
    accepted the reverse forward, and a 200 on the v2 base endpoint exists only if the
    forward reached a registry. A port that merely accepts connections proves neither.
    """
    command = ['ssh', *options(), guest,
               shlex.join(['python3', '-c', GUEST_PROBE, str(port)])]
    result = subprocess.run(command, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL,
                            text=True, timeout=60)
    if result.returncode != 0:
        return False
    try:
        return json.loads(result.stdout).get('registry') is True
    except ValueError:
        return False
def private_directory(path):
    """Create the per-VM control directory, refusing a symlinked or shared one."""
    try:
        entry = os.lstat(path)
    except FileNotFoundError:
        try:
            os.mkdir(path, 0o700)
            return True
        except OSError:
            return False
    except OSError:
        return False
    return stat.S_ISDIR(entry.st_mode) and not entry.st_mode & 0o077
def forget(socket):
    for path in (socket,):
        try:
            os.unlink(path)
        except OSError:
            pass
    try:
        os.rmdir(payload['forward_dir'])
    except OSError:
        pass
def stop_forward():
    """Close this VM's reverse forward and prove the socket is gone."""
    socket = str(Path(payload['forward_dir']) / 'forward.sock')
    if master_running(socket):
        control(socket, '-O', 'exit')
    for attempt in range(20):
        if not master_running(socket):
            break
        time.sleep(0.25)
    if master_running(socket):
        return False
    forget(socket)
    return True
def start_forward():
    """Expose this host's loopback port on the guest's loopback port, once per VM.

    The forward is a reverse one: the guest's sshd owns the listener and the connection
    comes back along this same client, so the delivery target is this host's loopback
    only because the client was never moved into the guest's namespace.
    """
    if shutil.which('socat') is None:
        return False
    if not private_directory(payload['forward_dir']):
        return False
    socket = str(Path(payload['forward_dir']) / 'forward.sock')
    if len(os.fspath(socket)) > 100:
        return False
    if master_running(socket):
        # Re-preparing must reuse one forward, never stack a second listener on the
        # guest's port. A master carrying a different port is closed before a new one.
        if reachable():
            return True
        control(socket, '-O', 'exit')
    command = ['ssh', '-M', '-S', socket, '-f', '-N',
               # ControlPersist is deliberately off: with no second session the master
               # exits when the guest's connection drops, which bounds the forward to
               # the lease that opened it instead of leaving it behind on the host.
               '-o', 'ControlPersist=no',
               # Without this a refused bind backgrounds as a silent success and the
               # job pays full Docker Hub latency while the operator believes otherwise.
               '-o', 'ExitOnForwardFailure=yes', *options(durable=True),
               '-R', '127.0.0.1:%d:127.0.0.1:%d' % (port, port), guest]
    if subprocess.run(command, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
                      timeout=60).returncode != 0:
        return False
    return master_running(socket) and reachable()
if port and action == 'prepare' and not start_forward():
    sys.exit(1)
request = dict(payload)
# Preserve the original namespace transport when the mirror is disabled. Existing
# installations need socat only when they explicitly enable the reverse tunnel.
ssh_prefix = ['ssh'] if port else ['ip', 'netns', 'exec', namespace, 'ssh']
result = subprocess.run([*ssh_prefix, *options(), guest,
                         shlex.join(['python3', '-c', request.pop('guest_program')])],
                        input=json.dumps(request), text=True, stdout=subprocess.PIPE,
                        stderr=subprocess.DEVNULL, timeout=60)
if result.stdout:
    sys.stdout.write(result.stdout)
# Teardown runs after the guest program so its status stays the reported answer, and
# a controller that died earlier leaves the master to die with the guest's connection.
if port and action == 'stop' and not stop_forward():
    sys.exit(1)
sys.exit(result.returncode)
'''


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--host', required=True)
    parser.add_argument('--key-file', required=True, help='Guest administrative key on Metal')
    parser.add_argument('--known-hosts', required=True, help='Dedicated per-VM known-hosts prefix on Metal')
    parser.add_argument('action', choices=('prepare', 'status', 'bootstrap', 'collect', 'stop'))
    # Explicit operator opt-in, applied by the guest program during prepare only, and
    # only on an unregistered fresh clone. The passwordless sudo it grants is the
    # disposable Firecracker guest's own root: it reaches no Metal host path, no
    # network namespace and no control-plane or GitHub credential.
    parser.add_argument('--allow-guest-sudo', action='store_true',
                        help='Give a fresh guest runner account passwordless sudo inside that guest')
    # Explicit operator opt-in to select the guest kernel's tsc clocksource during
    # prepare. Firecracker passes no pvclock page, so the guest's kvm-clock is
    # MSR-backed, the vDSO cannot serve clock_gettime, and every clock read in
    # every process becomes a syscall; selecting tsc that the kernel already
    # offers avoids it. This is the disposable guest's own kernel setting: it
    # changes no Metal host value, no network namespace, and no credential, and it
    # is applied before registration so no job observes a partial change.
    parser.add_argument('--prefer-guest-tsc', action='store_true',
                        help='Select the tsc clocksource in a fresh guest to keep clock reads out of the kernel')
    # Explicit operator opt-in for an anonymous, read-only container image mirror. It
    # opens one reverse SSH forward from the guest's loopback to this Metal host's
    # loopback and points the guest's Docker daemon at it. No host port is published,
    # no credential is written anywhere, and nothing is pulled into the image.
    # Explicit operator opt-in to publish container ports through the kernel's
    # netfilter DNAT instead of Docker's docker-proxy userspace process. Measured
    # on this host's guest, that relay costs about 110 microseconds per request on
    # an already-open connection. A GitHub-hosted runner pays the same hop, so this
    # closes ground rather than moving a baseline. The guest's own Docker daemon
    # only, applied before registration, with no published port left behind and
    # nothing installed or downloaded: the proof uses an image the baked runner
    # image already carries.
    parser.add_argument('--kernel-published-ports', action='store_true',
                        help='Publish container ports through kernel DNAT instead of a userspace proxy')
    parser.add_argument('--registry-mirror-port', type=int,
                        help='Serve an anonymous registry mirror to fresh guests on this loopback port')
    # Explicit operator opt-in for a RAM-backed package cache. The filesystem lives
    # inside the disposable guest and dies with the lease: it holds only the yarn and
    # uv downloads one job makes, so it moves those writes out of the guest's disk
    # without touching the Metal host, the guest workspace or any file that outlives
    # the run. A preparation that cannot prove the memory reserve fails instead.
    parser.add_argument('--cache-tmpfs-mib', type=int,
                        help='Back a fresh guest package cache with this many MiB of RAM')
    # The same opt-in for the directory the guest runner checks every job out into, so
    # the trial can move that directory to RAM instead. Identical bounds, ownership and
    # reserve: one measured variable at a time, and this flag is exclusive with the cache.
    parser.add_argument('--workspace-tmpfs-mib', type=int,
                        help="Back a fresh guest's job workspace with this many MiB of RAM")
    parser.add_argument('--vm-id', required=True)
    parser.add_argument('--runner-name', required=True)
    parser.add_argument('--deadline', help='Absolute RFC3339 deadline for bootstrap')
    parser.add_argument('--diagnostics-dir', type=Path, help='Private local directory for collect')
    args = parser.parse_args()
    if not re.fullmatch(r'vm-[0-9]+', args.vm_id):
        parser.error('Expected an Atlas vm-<digits> identifier')
    if not re.fullmatch(r'runway-[a-z0-9-]{1,56}', args.runner_name):
        parser.error('Expected a runway-prefixed runner name')
    if not re.fullmatch(r'[A-Za-z0-9_][A-Za-z0-9_.@-]*', args.host):
        parser.error('Expected an SSH host, optionally prefixed with a user')
    if any(not value.startswith('/') or '\n' in value for value in (args.key_file, args.known_hosts)):
        parser.error('Host key and known-hosts paths must be absolute')
    if args.action == 'collect' and not args.diagnostics_dir:
        parser.error('collect requires --diagnostics-dir')
    if args.registry_mirror_port is not None and not 1024 <= args.registry_mirror_port <= 65535:
        parser.error('Registry mirror port must be between 1024 and 65535')
    if args.cache_tmpfs_mib is not None and not 128 <= args.cache_tmpfs_mib <= 16384:
        parser.error('Cache tmpfs must be between 128 and 16384 MiB')
    if args.workspace_tmpfs_mib is not None and not 128 <= args.workspace_tmpfs_mib <= 16384:
        parser.error('Workspace tmpfs must be between 128 and 16384 MiB')
    if args.cache_tmpfs_mib is not None and args.workspace_tmpfs_mib is not None:
        # Refused here, before a key or a channel is used: the two opt-ins reserve memory
        # against one 6 GiB guest, and a trial that enabled both would measure their sum
        # instead of either one.
        parser.error('The cache and workspace tmpfs are exclusive: reserve RAM for one or the other')
    if args.deadline:
        try:
            deadline = datetime.fromisoformat(args.deadline)
            if deadline.tzinfo is None or deadline <= datetime.now(UTC):
                raise ValueError()
        except ValueError:
            parser.error('deadline must be a future timezone-aware RFC3339 timestamp')
    secret = sys.stdin.read(65537).strip() if args.action == 'bootstrap' else ''
    if args.action == 'bootstrap' and (not secret or len(secret) > 65536 or '\n' in secret):
        parser.error('Expected one JIT configuration on stdin')
    payload = dict(action=args.action, runner_name=args.runner_name, vm_id=args.vm_id,
                   key_file=args.key_file, known_hosts=args.known_hosts + '-' + args.vm_id,
                   allow_guest_sudo=args.allow_guest_sudo,
                   prefer_tsc_clocksource=args.prefer_guest_tsc,
                   kernel_publish=args.kernel_published_ports,
                   guest_program=GUEST_PROGRAM)
    if args.registry_mirror_port:
        payload['mirror_port'] = args.registry_mirror_port
        # One private control directory per VM, beside the known-hosts prefix the
        # controller already reserves on Metal. It holds only the reverse forward's
        # control socket, which is bound to loopback and carries no key material.
        payload['forward_dir'] = str(Path(args.known_hosts).parent
                                     / ('runway-forward-' + args.vm_id))
    if args.cache_tmpfs_mib:
        payload['cache_tmpfs_mib'] = args.cache_tmpfs_mib
    if args.workspace_tmpfs_mib:
        payload['workspace_tmpfs_mib'] = args.workspace_tmpfs_mib
    if args.deadline:
        payload['deadline'] = args.deadline
    if args.action == 'bootstrap':
        payload.update(jit=secret, wrapper=(Path(__file__).resolve().parents[1] / 'guest/guest-runner.sh').read_text())
    try:
        # Trust boundary: the operator's agent authenticates us to Metal, but must never
        # reach it. Command-line -o is read before ~/.ssh/config and wins, so an operator
        # ForwardAgent yes cannot carry the agent across this hop.
        result = subprocess.run(['ssh', '-o', 'BatchMode=yes', '-o', 'ForwardAgent=no',
                                 '-o', 'ConnectTimeout=10',
                                 args.host, shlex.join(['python3', '-c', HOST_PROGRAM])],
                                input=json.dumps(payload), text=True, stdout=subprocess.PIPE,
                                stderr=subprocess.DEVNULL, timeout=75)
        if result.returncode != 0:
            print(json.dumps({'state': 'unknown'}))
            return 1
        status = json.loads(result.stdout)
        if status.get('state') not in ('installed', 'running', 'completed', 'unknown'):
            raise ValueError('Invalid guest status')
        if args.action == 'collect':
            directory = args.diagnostics_dir
            directory.mkdir(mode=0o700, parents=True, exist_ok=True)
            if directory.is_symlink() or directory.stat().st_mode & 0o077:
                raise ValueError('Diagnostics directory must be private')
            output = directory / (args.runner_name + '-' + args.vm_id + '.log')
            fd = os.open(output, os.O_WRONLY | os.O_CREAT | os.O_TRUNC | os.O_NOFOLLOW, 0o600)
            with os.fdopen(fd, 'w') as stream:
                os.fchmod(stream.fileno(), 0o600)
                stream.write(str(status.get('diagnostics', ''))[-131072:])
                stream.flush()
                os.fsync(stream.fileno())
        print(json.dumps({'state': status['state']}))
        return 0
    except (subprocess.TimeoutExpired, ValueError, OSError):
        print(json.dumps({'state': 'unknown'}))
        return 1


if __name__ == '__main__':
    sys.exit(main())
