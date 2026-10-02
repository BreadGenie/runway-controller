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
| Run | [36997401912](https://github.com/BreadGenie/frappe/actions/runs/36997401912) | [36997405257](https://github.com/BreadGenie/frappe/actions/runs/36997405257) |
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

### Changes shipped for the measured comparison

A new guest image was published for the follow-up measurement: a 4 GiB raw image carrying a verified toolcache, with 3.3 GiB used. Its raw SHA256 is `ac3c3f967524d1a7cd0b524cd398b44b6629c1af2d3bcb2f45ac70792d4c22e6`, published as Atlas image `62fee10c787e787e`. Three changes go with it:

1. **Baked profile.** Node 24.21.0, CPython 3.14.7 and the compiler and database client packages they need are provisioned in the image rather than per job. The measured target is the toolchain provisioning that previously cost roughly 9.8 s for Node and 18.5 s for CPython on every self-hosted job. Docker service images are not baked; the recipe no longer pulls them, so `postgres`, `mariadb` and `smtp4dev` are still pulled once per job exactly as before. This is an image-content change and does not alter the job's steps, services or commands.
2. **Aligned workspace.** The controller now requests the absolute work folder `/home/runner/work` instead of the runner-relative `_work`, so the guest workspace sits at the same depth as GitHub-hosted's and both sides write and read cache archives with the same prefix. This is a path-geometry correction, not a throughput change: it makes a cache entry restorable across runner types. The entry written under the old layout was unusable on both sides, so an operator cleared that superseded entry, id `8411122268`, before the GitHub-hosted regeneration produced a compatible archive.
3. **Toolcache environment.** The guest exports `RUNNER_TOOL_CACHE` so the runner actually reads the baked toolcache. Without it the runner defaults to `<work>/_tool`, which a baked image never populates, so change 1 would have been inert. The variable is adopted only when the prepared directory already exists and is writable, and an inherited value is always respected, so an unmodified bare image keeps the runner's own default.

The workflow also pins `python-version: '3.14.7'` and `node-version: '24.21.0'`, removing the last measured interpreter mismatch so both runner types resolve one toolchain and therefore one uv cache key.

No regional or self-hosted cache tier was implemented. The existing GitHub Actions cache service is still used unchanged; the corrections above are confined to image content, the workspace layout the controller requests, and the toolcache environment the guest sets.

### Measured result: optimized Runway against later GitHub-hosted

The follow-up pair ran the same job at the same commit, `4423d0a`, with the toolchain pins (`python-version: '3.14.7'`, `node-version: '24.21.0'`) on both sides. Both passed the full suite: `Tests: 708, Failing: 0, Errors: 0`.

| | Runway, original | Runway, optimized | GitHub-hosted, original | GitHub-hosted, later |
| --- | --- | --- | --- | --- |
| Run | [36997405257](https://github.com/BreadGenie/frappe/actions/runs/36997405257) | [37005204626](https://github.com/BreadGenie/frappe/actions/runs/37005204626) | [36997401912](https://github.com/BreadGenie/frappe/actions/runs/36997401912) | [37004189899](https://github.com/BreadGenie/frappe/actions/runs/37004189899) |
| Hardware | 3 vCPU / 6 GiB / 10 GiB disk | 3 vCPU / 6 GiB / 10 GiB disk | 4 vCPU / 16 GiB | 4 vCPU / 16 GiB |
| Yarn cache | warm | warm | unrestorable | warm |
| uv cache | warm (3.14.8) | cold (3.14.7) | cold (3.14.7) | warm (3.14.7) |
| Dispatch (`run_started_at` → job start) | 150 s | **41 s** | 12 s | 10 s |
| Job total (start → complete) | 745 s | **741 s** | 554 s | 428 s |
| Environment Setup | 207 s | **177 s** | 112 s | 111 s |
| Run Tests | 462 s | 479 s | 398 s | 281 s |
| Tests reported | 708 passing | 708 passing | 708 passing | 708 passing |

**The cache state is not matched across the pair, so this is not a fully warm-cache comparison.** The yarn cache was warm on both sides of the optimized Runway run and the later GitHub-hosted run, which is what makes the shared-cache result below meaningful. The uv cache was not: it was cold on the optimized Runway run, where the log records `No GitHub Actions cache found for key` for the `3.14.7` uv key and the post step then wrote it, while it was warm on the later GitHub-hosted run. The original Runway run was warm on a `3.14.8` uv key, so the interpreter pin moved that runner onto a different, then-cold namespace rather than improving on what it had. The two systems therefore did their setup work against different cache states, and the optimized Runway setup figure is not a like-for-like counterpart to the GitHub-hosted one.

Dispatch and job total are reported separately because they are disjoint: dispatch is time before the job started and is not part of the job total. An additional cache-regeneration run, [37003000586](https://github.com/BreadGenie/frappe/actions/runs/37003000586), was used to establish the entry state and is not counted as a performance data point.

**The job duration itself is flat, but the time from run dispatch to job completion is shorter.** On Runway the job total barely moved, 745 s to 741 s, while dispatch fell from 150 s to 41 s. Taken together, dispatch plus job fell from 895 s to 782 s, about 13 % less end to end. Setup improved by 30 s, from 207 s to 177 s; within the job that was offset by `Run Tests` rising from 462 s to 479 s and container initialization from 51 s to 58 s. Startup and dispatch, not job execution, is where the change landed.

The two systems' later runs are not otherwise comparable. The later GitHub-hosted run shows `Run Tests` falling from 398 s to 281 s, and the later Runway run shows it rising from 462 s to 479 s, on a phase none of these changes touch. Test time varied between runs on both systems; a single pair of runs cannot attribute that variation to the optimization or to anything else, and no cause is assigned to it here. The original GitHub-hosted run resolved CPython 3.14.7, the same interpreter as the later one, and installed the same applications, but the apt phase differs between them.

### Cache behaviour after the change

Every cache claim here was checked against the run logs rather than inferred.

| | GitHub-hosted, original | GitHub-hosted, later | Runway, optimized |
| --- | --- | --- | --- |
| CPython toolchain | image-baked | `0.11 s`, found in cache | `0.18 s`, found in `/opt/hostedtoolcache` |
| Node toolchain | image-baked | found in `/opt/hostedtoolcache` | found in `/opt/hostedtoolcache` |
| Yarn cache key | `…b3d5d3b` | `…b3d5d3b` | `…b3d5d3b` |
| Yarn archive size | 324 872 919 B | 323 096 006 B | 323 096 006 B |
| Yarn transfer | 1.38 s | 2.20 s | 23.44 s |
| Yarn extract | failed, `exit 2` | 1.03 s, clean | 9.63 s, clean |
| `Cannot mkdir` lines | 28 590 | **0** | **0** |
| uv cache | miss | hit, 16 852 858 B | miss, then saved |
| wkhtmltopdf cache | hit | hit | hit |

The decisive result is that both runner types now request the **same yarn cache key** and both restore it cleanly, with zero `Cannot mkdir` lines and no restore warning. The original GitHub-hosted run carried 28 590 of those errors while still concluding `success`; that failure mode is gone from both sides. Toolchain provisioning moved to the baked image and is now effectively free on Runway, where `setup-python` fell from 18.47 s to 0.18 s and `setup-node` from 41.27 s to 36.05 s.

Transfer remains the bottleneck on the self-hosted runner. The 308 MB yarn archive still costs 23.44 s to fetch at about 13.7 MB/s against roughly 140 MB/s on GitHub-hosted, and that fetch plus extract is now essentially all of the remaining `setup-node` time. Making the archive restorable removed a silent failure; it did not make the network faster. The uv cache missed on this Runway run and was written by the post step, so the 3.14.7 namespace is warm only from this run onward and its measured benefit does not appear in these numbers.

### Host and artifact conditions

The self-hosted host was prepared before the clean run: the sparse ZFS disk image backing was raised from 12 GiB to 20 GiB and the ARC limit set to 256 MiB. A `zpool trim` at 12:15:52, before the test phase began at 12:16:03, left about 11 GiB free on the host, roughly 9 GiB more than the 1.5 GiB free beforehand. Source build-tree cleanup retained the source archive and the built kernel. Guest disk occupancy was 3.3 GiB used with 6.1 GiB free initially, 7.1 GiB used with 2.3 GiB free after setup, and 7.2 GiB used with 2.2 GiB free at runtime, so setup is where the disk is consumed.

Two earlier attempts, [37004442985](https://github.com/BreadGenie/frappe/actions/runs/37004442985) and [37004967999](https://github.com/BreadGenie/frappe/actions/runs/37004967999), were cancelled on a definite capacity rejection before any VM existed. They are excluded from the benchmark because they measured admission, not execution.

The published raw image is `ac3c3f967524d1a7cd0b524cd398b44b6629c1af2d3bcb2f45ac70792d4c22e6`, Atlas image `62fee10c787e787e`, a 4 GiB image with a verified toolcache. The artifact in use still carries a 17 MB `wkhtmltox` `.deb` copied in by the earlier recipe; it was verified to be the same SHA the current recipe fetches, and the new recipe cleans that copy rather than relying on it. The build recipes are documented generically and are not claimed to reproduce an exact image.

### Cleanup after the measured run

The lease reached phase `Deleted` with JIT state `removed`, VM 17 was gone, GitHub reported zero remaining runners, and no Metal namespace or dataset remained. The pool was paused, the controller stopped, and the dedicated scale set 5 deleted.

### What this evidence does not establish

These are single runs, one per system, not a fleet benchmark, and the cache state was not uniform between them: the yarn cache was warm on both later runs, but the uv cache was cold on Runway and warm on GitHub-hosted, so this is not a fully warm-cache comparison and the uv benefit remains unmeasured until a Runway run finds that key present. A single pair of runs cannot attribute the observed test-time differences to the optimization or to any other cause. The two systems remain differently sized, 4 vCPU and 16 GiB against 3 vCPU and 6 GiB, so their totals are not directly comparable as a like-for-like figure; the useful comparison is each system against its own earlier run. Host tuning (ZFS sizing, ARC, trim) changed alongside the image, so the self-hosted pair differs in more than the baked profile.

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

The Frappe integration comparisons are single runs per system, so they carry no variance estimate and no repeated-sample confidence, and one pair of runs cannot attribute the observed test-time differences to any cause. Cache state was not uniform across them either: the yarn cache was warm on both later runs, while the uv cache was cold on Runway and warm on GitHub-hosted, so the pair is not a fully warm-cache comparison. Each system's two runs also differ in more than the intended change: the self-hosted pair additionally spans host tuning of ZFS sizing, the ARC limit and a `zpool trim`. The systems remain differently sized at 4 vCPU / 16 GiB against 3 vCPU / 6 GiB, so their totals do not rank either platform against the other; only each system's own two runs are comparable. The measured outcome on the self-hosted runner is shorter dispatch and shorter setup, with job duration essentially unchanged. Adoption of the baked toolcache can only be confirmed on a guest that has the prepared directory; where it is absent the guest deliberately leaves the runner's default in place, so a passing run there does not evidence the toolcache path. Cache sharing still depends on GitHub's own cache service being reachable from the guest.
