# Proof of concept results

In the initial execution experiment on 2026-10-02, two real GitHub workflows passed on separate fresh Atlas/Firecracker VMs. Each exercised a Docker Redis service and a Python Redis protocol probe. The final listener replay persisted successful completion before acknowledging GitHub. All trial VMs, GitHub runners, and the temporary scale set were removed.

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

## Durable pilot restart

The [durable controller run](https://github.com/BreadGenie/fc-ci-dogfood-spike/actions/runs/36980664391) passed using a baked runner image. The controller was killed with `SIGKILL` during a two-minute workflow step, then restarted with the same pool owner identity. Central incremented the ownership fence, and the controller resumed the existing lease and VM without a second reservation or JIT registration.

The stale GitHub message session briefly prevented reconnecting. Retrying recovered in about a minute in this trial; this is an observation, not a documented expiration guarantee. The job completed successfully, Central persisted `success`, the GitHub registration was removed, and the VM reached `deleted`. No trial guest namespace remained. A bounded guest journal was collected into a private file before deletion.

The image was built from a fresh, never-registered guest and separately verified before this run. Boot required no package installation. The published raw image is 4 GiB, with SHA256 `59def4967c665821432a612beb679f7543d984a9b4677e343972908ef1b53a50`.

With the controller stopped, a separate VM lease was reserved through the authenticated Central API with a 60-second deadline and no GitHub registration. Central's scheduled reaper advanced it from `vm_ready` to `deleted`, marked registration state `removed`, and left no guest namespace. No manual reconciliation call was used. The pool was then paused and the dedicated scale set removed; GitHub reported zero runners.

## Frappe fork dogfood

A real Frappe CI job was run through the controller for the first time. The repository was a BreadGenie Frappe fork on branch `runway-dogfood`, commit `bd2240f` based on `979b90732b3ee64dfb1586cb98db9a59e45e070a`, running the repository's own unchanged check `unittest frappe.tests.test_boilerplate.TestBoilerPlate.test_valid_ci_yaml` on Python 3.14 in a fresh Runway VM with two CPUs, 4 GiB RAM and a 4 GiB disk.

| Run | Result | What it established |
| --- | --- | --- |
| Attempt 1 | Failed | The baked guest's `runner` account had Docker but no passwordless sudo |
| [Attempt 2](https://github.com/BreadGenie/frappe/actions/runs/36986854455/job/110780787490) | Passed on 2026-10-02 | The same unchanged check ran in a fresh guest after that gap was closed |

Attempt 2 took about 119 seconds end to end, including installation of the full Frappe dependency set; the test itself reported 0.006 seconds. The missing sudo attempt 1 exposed is now an explicit operator opt-in during guest preparation, not a change to the image recipe or the workflow. The successful VM was removed automatically and its GitHub runner deregistered.

This is one focused unit check on one repository branch. It is not the full Frappe suite, which needs a database, and the recorded durations are not a performance benchmark.

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

The earlier experiments used operator orchestration and a local listener journal. The durable pilot now uses Central-backed leases and automatic lifecycle orchestration, with one live controller crash/restart trial. Unit tests cover explicit acquisition and failure boundaries; the live runs used direct assignment. This is not fleet-scale or exhaustive outage evidence.

The experiment did not validate tenant egress isolation for untrusted jobs, fork PRs, production billing, fleet scaling, shared caches, customer onboarding, PR previews, or deploy-on-push. Runtime credentials, raw runner diagnostics, and machine-specific configuration are excluded from this repository.
