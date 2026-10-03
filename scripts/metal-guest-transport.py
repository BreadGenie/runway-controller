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
import json, os, shutil, stat, subprocess, sys, tempfile
from datetime import UTC, datetime
from pathlib import Path
payload = json.load(sys.stdin)
action = payload['action']
name = payload['runner_name']
marker = Path('/var/lib/runway/bootstrap.json')
runner = Path('/home/runner/actions-runner')
sudoers = Path('/etc/sudoers.d/runway-runner')
sudoers_rule = 'runner ALL=(ALL) NOPASSWD: ALL\n'
daemon_config = Path('/etc/docker/daemon.json')
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
if action in ('status', 'prepare'):
    current = state()
    if action == 'prepare' and current == 'installed':
        # Operator opt-ins, and only while this is an unregistered fresh clone: any
        # resulting root is the disposable Firecracker guest's own, never Metal's.
        if payload.get('allow_guest_sudo') and not grant_runner_sudo():
            print(json.dumps({'state': 'unknown'}))
            sys.exit(1)
        if payload.get('mirror_port') and not enable_mirror(int(payload['mirror_port'])):
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
    # Explicit operator opt-in for an anonymous, read-only container image mirror. It
    # opens one reverse SSH forward from the guest's loopback to this Metal host's
    # loopback and points the guest's Docker daemon at it. No host port is published,
    # no credential is written anywhere, and nothing is pulled into the image.
    parser.add_argument('--registry-mirror-port', type=int,
                        help='Serve an anonymous registry mirror to fresh guests on this loopback port')
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
                   allow_guest_sudo=args.allow_guest_sudo, guest_program=GUEST_PROGRAM)
    if args.registry_mirror_port:
        payload['mirror_port'] = args.registry_mirror_port
        # One private control directory per VM, beside the known-hosts prefix the
        # controller already reserves on Metal. It holds only the reverse forward's
        # control socket, which is bound to loopback and carries no key material.
        payload['forward_dir'] = str(Path(args.known_hosts).parent
                                     / ('runway-forward-' + args.vm_id))
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
