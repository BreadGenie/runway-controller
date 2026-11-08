# Standalone guest launcher

See the [standalone spec](../../specs/runway-regional-cache.md) for authority,
cache routing and trust requirements; [deployment](../blr-garage/README.md)
installs the launcher and guest helpers.

```sh
export RUNWAY_REPOSITORY=BreadGenie/frappe RUNWAY_RUNNER_LABEL=runway-cache-test-1104 RUNWAY_SCALE_SET_ID=39
runway-standalone-guest launch rc-a 1
runway-standalone-guest launch rc-x 2 --no-runner
runway-standalone-guest ssh rc-a 'journalctl -u runway-runner -n 20'
runway-standalone-guest teardown rc-a
runway-standalone-guest teardown rc-x
runway-standalone-guest reap
```

Distinct simultaneous guests need distinct indexes (1–9). The launcher clones
the configured image, injects this branch's cache helpers, sets up networking,
starts cache-host and Firecracker, registers a JIT runner and writes its binding.
Launch failures clean up partial resources. Teardown removes authority first,
then the runner, VM, host service, network and clone. Reap removes stopped VMs,
including those stopped by RuntimeMaxSec. Guest hours cannot exceed the host unit.

Guests boot only from the launcher's own image store (`metal/runway-images/NAME`
and `/var/lib/runway-standalone-images/NAME/vmlinux`). Metal prunes images and
kernels it does not use, and newer Metal moves kernels out of the image
directory, so `prepare-image` copies the disk and kernel once, accepting either
Metal layout. It then installs the official Actions runner (version and
archive digest pinned in the launcher, as in `guest/install-guest-runner.sh`)
over the image's runner, removes the retired `job-gate` and `cache-setup`
helpers, and refuses the image unless `bin/Runner.Worker.dll` is the official
build. A Metal image may carry a patched runner; the prepared guest never does.
Preparing needs HTTPS access to the GitHub release download:

```sh
runway-standalone-guest prepare-image regional-cache-node-v2 \
  sha256:f0dc9f17c4d910a057a017eb613e97ff093b34e686114937869f10ec07f17ade
```

Set RUNWAY_GUEST_IMAGE to select another prepared image.

Guests boot with `clocksource=tsc` by default. On a nested-KVM host such as
blr-1, kvm-clock has no stable flag, so every `clock_gettime` in the guest is a
syscall (about 4x slower). The guest keeps its TSC watchdog. Set
`RUNWAY_GUEST_CLOCKSOURCE=kvm-clock` to opt out, or set it empty to leave the
choice to the guest kernel.

Standalone guests run through `/usr/bin/jailer` in the existing systemd unit,
using slot accounts `runway-fc-1` through `runway-fc-9`. Each jail lives at
`/srv/runway-jail/firecracker/VM/root`, with a kernel hardlink (copy across
filesystems), its own root-drive device node, and jail-relative config paths
`/vmlinux`, `/dev/rootfs` and `/vsock/v.sock`. The VM socket directory is
bind-mounted at `vsock/`, owned by root and the slot group with mode `0750`;
cache-host sockets use that group and mode `0660`. Binding v2 records
`jail_uid` and `jail_gid`: cache-host requires the peer PID, uid and gid to
match, and the gid must also match the socket directory. V1 bindings are refused.
The launcher verifies the jail's executable path and its digest against
`/usr/bin/firecracker` before writing the binding. Teardown, reap and failed
launch cleanup stop the unit, unmount `vsock/`, then remove the jail with
`rm -rf --one-file-system`. No extra PID namespace or jailer cgroup is used.

`deploy/blr-garage/install.sh` creates the nine system accounts (nologin, no
home) and the root-owned `0711` jail base. It refuses installation while any
binding file exists in `/run/runway-cache/bindings`; tear down guests first.
Rollback requires restoring both the launcher and runway `.previous` binaries.
