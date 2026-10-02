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

## Frappe integration benchmark history

### First controlled-shape comparison: same commit, two runner types

A single revision was deliberately run through the existing, unmodified `Server` workflow on both runner types on 2026-10-02, so the comparison exercises the repository's own integration job rather than a substitute check. Both runs passed the same suite: `Tests: 708, Failing: 0, Errors: 0`.

| | GitHub-hosted | Runway self-hosted |
| --- | --- | --- |
| Commit | `de83ae2391368129df448f5fde18ca7fc873b2e3` | `de83ae2391368129df448f5fde18ca7fc873b2e3` |
| [Run](https://github.com/BreadGenie/frappe/actions/runs/36997401912) | [36997401912](https://github.com/BreadGenie/frappe/actions/runs/36997401912) | [36997405257](https://github.com/BreadGenie/frappe/actions/runs/36997405257) |
| [Job](https://github.com/BreadGenie/frappe/actions/runs/36997401912/job/110807206183) | [110807206183](https://github.com/BreadGenie/frappe/actions/runs/36997401912/job/110807206183) | [110807218235](https://github.com/BreadGenie/frappe/actions/runs/36997405257/job/110807218235) |
| Label | `ubuntu-latest` | `runway-frappe` |
| CPU / RAM | 4 vCPU / 16 GiB | 3 vCPU / 6 GiB |
| CPython resolved | 3.14.7 | 3.14.8 |
| Dispatch (`run_started_at` → job start) | 12 s | 150 s |
| Environment Setup | 112 s | 207 s |
| Run Tests | 398 s | 462 s |
| Job total (start → complete) | 554 s | 745 s |

Dispatch here is measured from the run's `run_started_at` to the job's `started_at`, so it includes time the job spent waiting for capacity, not just runner registration.

**This pair is not a controlled benchmark and must not be read as one.** Three independent variables differ. Hardware is not matched — Runway had one fewer vCPU and roughly a third of the RAM. The interpreter was not matched, because the workflow requested `python-version: '3.14'`, which resolves to the newest 3.14.x present in each runner's own toolcache, and the two tool caches were at different patches. Most importantly, the two runs did not experience equivalent artifact-cache state: both runners requested the identical yarn cache key, but its contents were only usable under one of the two workspace layouts.

### Why the cache was asymmetric

`actions/setup-node` with `cache: yarn` archives the yarn store as paths relative to the checkout workspace, so the `../../` prefix is fixed into every archive member. The cache key encodes OS, architecture, package manager and the `yarn.lock` hash, but not workspace depth, so both runner types shared one entry rather than being namespaced apart. Under the self-hosted layout the prefix resolved onto the runner home and restored; under the GitHub-hosted layout the identical prefix resolved one directory higher, to a path that did not exist. `tar` does not create parent directories, so every member failed: the GitHub-hosted log contains 28 590 such errors, followed by a restore warning and `yarn cache is not found`, while the step still concluded `success`.

The measured consequence was confined to setup. On GitHub-hosted, yarn refetched packages during `bench init` because its restore had produced nothing, while the same install on Runway resolved from the restored store. The `Run Tests` phase, by contrast, tracked the other runner closely for the whole 708-test suite and showed no comparable discontinuity, so the gap between the two runs is dominated by setup and dispatch rather than by test execution.

### Changes shipped for the next comparison

A new guest image was published for the follow-up measurement: a 4 GiB raw image carrying a verified toolcache, with 3.3 GiB used. Its raw SHA256 is `ac3c3f967524d1a7cd0b524cd398b44b6629c1af2d3bcb2f45ac70792d4c22e6` (digest `62fee…`). Three changes go with it:

1. **Baked profile.** Node 24.21.0, CPython 3.14.7 and the service container images are provisioned in the image rather than per job. The measured target is the toolchain provisioning that previously cost roughly 9.8 s for Node and 18.5 s for CPython on every self-hosted job, plus repeated service-image pulls. This is an image-content change and does not alter the job's steps, services or commands.
2. **Aligned workspace.** The controller now requests the absolute work folder `/home/runner/work` instead of the runner-relative `_work`, so the guest workspace sits at the same depth as GitHub-hosted's and both sides write and read cache archives with the same prefix. This is a path-geometry correction, not a throughput change: it makes a cache entry restorable across runner types. The entry already written under the old layout is unusable on both sides and still has to be cleared before the alignment shows any benefit.
3. **Toolcache environment.** The guest exports `RUNNER_TOOL_CACHE` so the runner actually reads the baked toolcache. Without it the runner defaults to `<work>/_tool`, which a baked image never populates, so change 1 would have been inert. The variable is adopted only when the prepared directory already exists and is writable, and an inherited value is always respected, so an unmodified bare image keeps the runner's own default.

The workflow also pins `python-version: '3.14.7'` and `node-version: '24.21.0'`, removing the last measured interpreter mismatch so both runner types resolve one toolchain and therefore one uv cache key.

No regional or self-hosted cache tier was implemented. The existing GitHub Actions cache service is still used unchanged; the corrections above are confined to image content, the workspace layout the controller requests, and the toolcache environment the guest sets.

**No optimized result is recorded yet.** The next entry is a baseline run, [37003000586](https://github.com/BreadGenie/frappe/actions/runs/37003000586) at commit `4423d0a`, which re-measures the same job on the new image so the two recorded pairs share an image and a commit. Optimized evidence will be appended only once that run and its comparison have actually executed; no speedup is claimed, projected or implied by the changes above.

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

The Frappe integration comparison is a single run per runner type, so it carries no variance estimate and no repeated-sample confidence. Its two halves differ in CPU count, RAM and CPython patch as well as in cache state, and it therefore ranks nothing; it is a baseline to be compared against, not a result. The workspace alignment makes cache archives portable between runner types, but the existing yarn cache entry is still in the superseded layout and unusable until an operator clears it. Adoption of the baked toolcache can only be confirmed on a guest that actually has the prepared directory; where it is absent the guest deliberately leaves the runner's default in place, so a passing run there does not evidence the toolcache path. No optimized measurement is yet recorded for the new image.
