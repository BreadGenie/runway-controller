#!/usr/bin/env python3
"""Bootstrap one disposable Metal guest over an operator's SSH channel."""

import argparse
import base64
from datetime import UTC, datetime
import json
import os
import re
import shlex
import subprocess
import sys
from pathlib import Path

# Ship the executable guest module unchanged; the host adapter only carries it.
GUEST_PROGRAM = (Path(__file__).resolve().parents[1] / 'guest/guest_agent.py').read_text()


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
                        stderr=None if action == 'prepare' and payload.get('regional_cache') else subprocess.DEVNULL, timeout=60)
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
    parser.add_argument('--regional-cache', help='Pinned TLS cache internal-name:port (prepare only)')
    parser.add_argument('--regional-cache-ca', type=Path, help='CA PEM on the controller')
    parser.add_argument('--regional-cache-node', type=Path, help='Development override: cache-node binary on the controller')
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
    parser.add_argument('--registry-mirror-port', type=int,
                        help='Serve an anonymous registry mirror to fresh guests on this loopback port')
    # Explicit operator opt-in to publish container ports through the kernel's
    # netfilter DNAT instead of Docker's docker-proxy userspace process. Measured on
    # this host's guest, that relay costs 101 to 116 microseconds per request on an
    # already-open connection, and two live jobs with this set shortened the 708-test
    # phase by 57.92 seconds against five runs that set neither. A GitHub-hosted
    # runner pays the same hop, so this closes ground rather than moving a baseline.
    # The guest's own Docker daemon only, applied before registration, with no
    # published port left behind and nothing installed or downloaded: the proof uses
    # an image the baked runner image already carries.
    parser.add_argument('--kernel-published-ports', action='store_true',
                        help='Publish container ports through kernel DNAT instead of a userspace proxy')
    # Report how long each preparation step took, in the status document's timings_ms.
    # Off by default so the status a controller parses is unchanged unless asked for.
    parser.add_argument('--timings', action='store_true',
                        help='Report per-step preparation timings in the status document')
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
    if any((args.regional_cache, args.regional_cache_ca, args.regional_cache_node)):
        if not all((args.regional_cache, args.regional_cache_ca)):
            parser.error('Regional cache requires --regional-cache and --regional-cache-ca')
        if not re.fullmatch(r'internal-[a-z0-9][a-z0-9.-]*:[0-9]+', args.regional_cache) or not 1 <= int(args.regional_cache.rsplit(':', 1)[1]) <= 65535:
            parser.error('Regional cache requires internal-name:port')
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
                   timings=args.timings,
                   key_file=args.key_file, known_hosts=args.known_hosts + '-' + args.vm_id,
                   allow_guest_sudo=args.allow_guest_sudo,
                   prefer_tsc_clocksource=args.prefer_guest_tsc,
                   kernel_publish=args.kernel_published_ports,
                   guest_program=GUEST_PROGRAM)
    if args.regional_cache and args.action == 'prepare':
        repo = Path(__file__).resolve().parents[1]
        sources = {'ca': args.regional_cache_ca,
                   'allowlist': repo / 'cmd/cache-node/allowlist.json'}
        if args.regional_cache_node:
            sources['node'] = args.regional_cache_node
        try:
            bundle = {key: base64.b64encode(path.read_bytes()).decode('ascii') for key, path in sources.items()}
        except OSError:
            parser.error('Cannot read regional cache files')
        bundle['service'] = args.regional_cache
        payload['regional_cache'] = bundle
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
                                stderr=None if args.action == 'prepare' and args.regional_cache else subprocess.DEVNULL, timeout=75)
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
        # The guest's document is re-serialised rather than forwarded, so anything it
        # reports beyond the state is dropped here unless it is asked for. That is what a
        # silent measurement looks like: the guest measured, and this line threw it away.
        document = {'state': status['state']}
        if args.timings and status.get('timings_ms'):
            document['timings_ms'] = status['timings_ms']
        print(json.dumps(document))
        return 0
    except (subprocess.TimeoutExpired, ValueError, OSError):
        print(json.dumps({'state': 'unknown'}))
        return 1


if __name__ == '__main__':
    sys.exit(main())
