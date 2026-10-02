# Proof of concept results

On 2026-10-02, two real GitHub workflows passed on separate fresh Atlas/Firecracker VMs. Each exercised a Docker Redis service and a Python Redis protocol probe. The final listener replay persisted successful completion before acknowledging GitHub. All trial VMs, GitHub runners, and the temporary scale set were removed.

The evidence below comes from a private scratch repository. Links require access; it was not a migration of a Frappe-owned CI workflow.

## Live runs

| Run | Result | What it established |
| --- | --- | --- |
| [Initial attempt](https://github.com/BreadGenie/fc-ci-dogfood-spike/actions/runs/36972877824) | Failed during Docker service setup | GitHub dispatched to the guest; the kernel lacked required raw netfilter support |
| [Second attempt](https://github.com/BreadGenie/fc-ci-dogfood-spike/actions/runs/36974249750) | Passed | Corrected kernel, official runner, and Docker Redis service worked in a fresh VM |
| [Third attempt](https://github.com/BreadGenie/fc-ci-dogfood-spike/actions/runs/36974691601) | Passed | Another fresh VM worked; captured events and redelivery established the actual listener contract |

The successful guests used Ubuntu 24.04, one virtual CPU, 2 GiB RAM, a 4 GiB root disk, runner 2.337.0, and a rebuilt Linux 5.10.223 kernel. This was a small compatibility check. It did not test heavy Frappe integration jobs or GitHub-hosted image parity.

## Fixes and recovery evidence

The initial guest kernel booted but lacked IPv4/IPv6 raw tables and the CT target needed by Docker's published service ports. A pinned kernel recipe enabled the required built-ins. A guest-local Redis port probe passed before the two successful workflows.

Actual GitHub events carried `runnerRequestId: 0`, a stable job UUID, the expected runner name/ID, and completion result `succeeded`. The listener initially assumed a positive request ID. After correction, an unacknowledged start/completion message was redelivered; the same journal recorded the result and the listener acknowledged it and exited zero.

The receipt records `assigned: true`, `acquired: false`, `raw_result: succeeded`, and normalized `result: success`. This proves live direct assignment and durable receipt replay. It does not prove the explicit `AcquireJobs` path or general fleet crash recovery.

Atlas cleanup initially needed scheduler registration repaired after an interrupted install. The bench's default 240-second tick also delayed Atlas's ten-second tasks. With a ten-second tick, final VM deletion automatically reached tenant-scoped HTTP `404` about 3.12 seconds after acceptance, without a manual reconciliation call. This is one observation, not a service-level guarantee.

Separate checks confirmed zero remaining GitHub runners, scale-set absence, and no trial Metal guest namespaces or VM datasets. Earlier infrastructure checks also verified SSH, outbound HTTPS, daemon restart survival, VM reboot, and persistent guest disk data.

## Artifact provenance

| Input | Revision or digest |
| --- | --- |
| Atlas | `339735de16471e90232cf8b1ff840696ee830ad9` |
| Central | `6676d3178943d332c288fc55c04e6f44a8d01732` |
| GitHub scale-set client | `e6daac702355cdb5b880b4fbdcf6d85dcd9e48e5` |
| Amazon Linux kernel source | `8c3eeb0cc41e3fa4e5e27e29c557124ed1a1c451` |
| Firecracker base configuration | `791763f545dd73c1f5f22c67dd314009ad8047d9` |
| Built kernel SHA256 | `7ab9d37ec5c01df4b19a60da69ba502993644780ced1175b3ad5b114d8151c77` |
| Final kernel config SHA256 | `e9c6a6a5b99f4f1ac46de943c159b531a32dbc3de25277c511e0cbdfbbd1f611` |

The recorded kernel was built with GCC 15.2.0 using explicit GNU11 mode. Binary hashes describe that build; they do not assert bit-for-bit reproducibility across compilers. The tested Atlas installation also used a small lazy Route53-client initialization fix so Generic-provider settings could be configured without AWS credentials.

## Limits

VM creation, guest preparation, registration, and cleanup were coordinated by an operator. The listener's local journal is not a durable multi-worker controller. Unit tests cover explicit acquisition and failure boundaries; the live runs used direct assignment.

The experiment did not validate tenant egress isolation for untrusted jobs, fork PRs, production billing, fleet scaling, shared caches, customer onboarding, PR previews, or deploy-on-push. Runtime credentials, raw runner diagnostics, and machine-specific configuration are excluded from this repository.
