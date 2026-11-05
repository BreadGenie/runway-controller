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
A Firecracker jailer remains a follow-up.

The default `regional-cache-review-20261105` snapshot was cloned from the
preserved `sha256:f0dc9f17c4d910a057a017eb613e97ff093b34e686114937869f10ec07f17ade`
image and updated with the rebuilt Runner.Worker (184 focused L0 tests passed).
Set RUNWAY_GUEST_IMAGE to select another prepared image. Rebuild recipe and
worker hashes are in the review evidence directory named in the validation doc.
