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
Metal layout. Choose a base with the 1025 administrative gate only; do not
reuse the retired regional-cache Worker build:

```sh
runway-standalone-guest prepare-image regional-cache-node-v2 \
  sha256:f0dc9f17c4d910a057a017eb613e97ff093b34e686114937869f10ec07f17ade
```

Set RUNWAY_GUEST_IMAGE to select another prepared image.
