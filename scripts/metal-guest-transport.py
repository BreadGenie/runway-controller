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
import json, os, shutil, stat, subprocess, sys
from datetime import UTC, datetime
from pathlib import Path
payload = json.load(sys.stdin)
action = payload['action']
name = payload['runner_name']
marker = Path('/var/lib/runway/bootstrap.json')
runner = Path('/home/runner/actions-runner')
sudoers = Path('/etc/sudoers.d/runway-runner')
sudoers_rule = 'runner ALL=(ALL) NOPASSWD: ALL\n'
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
if action in ('status', 'prepare'):
    current = state()
    if action == 'prepare' and current == 'installed' and payload.get('allow_guest_sudo'):
        # Operator opt-in, and only while this is an unregistered fresh clone: the
        # resulting root is the disposable Firecracker guest's, never Metal's.
        if not grant_runner_sudo():
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
import json, shlex, subprocess, sys
payload = json.load(sys.stdin)
# Trust boundary: the guest is reachable only with the pool-pinned administrative key.
# ForwardAgent=no keeps any agent offered by Metal's user ssh config out of the guest,
# and IdentitiesOnly=yes stops that agent's or Metal's default keys authenticating here.
command = ['ip', 'netns', 'exec', 'metal-' + payload['vm_id'], 'ssh',
           '-o', 'BatchMode=yes', '-o', 'ForwardAgent=no', '-o', 'IdentitiesOnly=yes',
           '-o', 'StrictHostKeyChecking=accept-new',
           '-o', 'ConnectTimeout=10', '-o', 'UserKnownHostsFile=' + payload['known_hosts'],
           '-i', payload['key_file'], 'root@172.16.0.2',
           shlex.join(['python3', '-c', payload.pop('guest_program')])]
result = subprocess.run(command, input=json.dumps(payload), text=True,
                        stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, timeout=60)
if result.stdout:
    sys.stdout.write(result.stdout)
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
