# Frappe runner performance

Runway targets existing GitHub Actions workflows: the same source revision, service
images, tests and coverage, inside a fresh disposable VM. These measurements use
Frappe's existing MariaDB integration shard at
`4423d0a362a702a20f4eee68470ef146081726b0`, with 708 tests and coverage enabled.

## Same-day baseline

Both runs were dispatched on 2026-10-03, two seconds apart.

| Phase | [GitHub](https://github.com/BreadGenie/frappe/actions/runs/37106019699) | [Runway](https://github.com/BreadGenie/frappe/actions/runs/37106017473) |
| --- | ---: | ---: |
| Dispatch to job start | 10 s | 65 s |
| Job | 512 s | 749 s |
| Initialize containers | 20 s | 45 s |
| Environment Setup | 93 s | 191 s |
| Run Tests | 389 s | 487 s |
| Tests passed | 708 | 708 |

Runway uses a 3 vCPU, 6 GiB RAM, 10 GiB disk guest on the existing 4 vCPU,
8 GiB host in Bangalore (Intel Xeon Platinum 8280 at 2.70 GHz). GitHub's job
reported 4 vCPU and 16 GiB. Both use Python 3.14.7 and Node 24.21.0.
A previous GitHub run completed in 428 seconds; repeated samples are needed to
separate an optimization's effect from host and network variation.

## What the setup logs show

Nested actions emit duration markers. Rounded durations for this pair:

| Action or subphase | GitHub | Runway |
| --- | ---: | ---: |
| setup-node, including Yarn cache | 5.75 s | 37.52 s |
| setup-uv | 3.35 s | 24.05 s |
| wkhtmltox cache | 0.84 s | 10.39 s |
| bench init | 9.78 s | 25.44 s |
| site creation and asset build | 33.32 s | 40.35 s |
| system dependencies and bench tool | 32.82 s | 37.13 s |

The UV cache now hits on Runway. Cache lookup, download and extraction still have
substantial overhead. Intervals from the cache-hit log to its size log include
more than pure payload transfer and must not be treated as precise network rates.
The unchanged workflow restores these caches even when files already exist, so
preloading them alone cannot eliminate the restore step.

An isolated fresh VM downloaded the 17 MB wkhtmltox release in 0.475 seconds
(host: 0.583 seconds), and completed 200 sequential 4 KiB fsyncs in 0.296 seconds.
These probes argue against a general network or fsync bottleneck; they do not
prove that every workflow request or filesystem operation is fast.

## Test execution

Console timestamps suggest a broad slowdown across classes: 122 of 123 matched
classes were slower on Runway. For classes lasting at least 0.5 seconds, the median
ratio was approximately 1.32. These are estimates from log timestamps, including
fixture work and output buffering, rather than instrumented function durations.
The command tests account for the largest class gap (about 30 seconds).

Frappe executes the selected shard's modules sequentially. More guest cores alone
will not necessarily speed up this phase. MariaDB already stores its data on tmpfs
in this workflow, and coverage already uses Python's SysMonitor support. Disabling
coverage or changing the selected tests would invalidate the comparison.

The baked Python build has PGO enabled; it does not have LTO, a tail-call interpreter
or JIT enabled. A compatible optimized build is a candidate for a separate controlled
experiment, not an established speedup or evidence that hardware must be replaced.

## Container cache experiment

The first optimization under test is an anonymous Docker Hub pull-through cache
on the Metal host's loopback interface, reached by a per-VM reverse SSH tunnel.
It preserves the workflow's normal `docker pull` commands and checks the service
image digests. See [container-cache.md](container-cache.md).

The [mirror trial](https://github.com/BreadGenie/frappe/actions/runs/37107646777)
passed all 708 tests with coverage. Container startup took 42 seconds, setup 202
seconds, tests 492 seconds, and the job 762 seconds (dispatch: 44 seconds).
Container startup improved by only 3 seconds and the job was 13 seconds slower
than baseline. This does not establish a useful overall gain.
The [paired GitHub run](https://github.com/BreadGenie/frappe/actions/runs/37107648283)
took 345 seconds, including 24 seconds for containers, 77 seconds for setup and
236 seconds for tests. This reinforces the need to report multiple samples.

The baked-image [trial](https://github.com/BreadGenie/frappe/actions/runs/37111170787)
passed all 708 tests with coverage. It used Atlas image `cffdaa3f24a245ca`, a 7 GiB
artifact (raw SHA256 `ce4ffa2e7f8ce2b50e13ffd9bae5222c90dfb0e7758477e885bf044951979c45`).
Container startup took 16 seconds, setup 171 seconds, tests 484 seconds and the job
700 seconds (dispatch: 44 seconds). This is 49 seconds faster than the baseline,
about 6.5%; the test phase itself remained almost unchanged.
The [paired GitHub run](https://github.com/BreadGenie/frappe/actions/runs/37111172701)
took 440 seconds: containers 32 seconds, setup 91 seconds and tests 308 seconds.
Runway has not reached overall timing parity.

The artifact came from never-registered builder VM21. Its offline export required
reviewed repair of an invalid systemd socket entry and allocation counters before
passing the finalizer; the original compressed export was retained. A separate
fresh VM verified tool versions, fresh identity, all three digests, cached pulls
and Redis PONG through a published Docker port. Both VMs and staging were deleted.
The workflow still pulls its declared tags, so a changed tag can fetch updated
content instead of silently using an old image.

## Reproducing the analysis

Download logs with `gh run view <id> --repo BreadGenie/frappe --log`, then run:

```sh
python3 scripts/profile-frappe-run.py compare github.log runway.log
python3 scripts/profile-frappe-run.py steps runway.log
```

Logs and sampler captures may contain private workflow information. Keep raw
captures outside the repository; publish only the relevant aggregate measurements.

## Bounded package cache in RAM

The transport accepts `--cache-tmpfs-mib 2048` as an explicit opt-in. During
preparation of an unregistered guest, it mounts `/home/runner/.cache` on a named
2 GiB tmpfs. The default remains a disk-backed cache. The guest must report at
least the requested capacity plus 2 GiB through `MemAvailable`; otherwise
preparation fails. The CLI range of 128–16384 MiB is not the admission ceiling:
for example, a guest reporting 5100 MiB available can admit at most 3052 MiB.
A refusal currently reports the transport’s generic `unknown` state; inspect
`/proc/meminfo` and `findmnt` when diagnosing a failed preparation.
The cap limits filesystem growth, but memory is consumed as
files are written and competes with the database and test processes.

The mount stays executable for native package binaries and uses `nosuid,nodev`.
It belongs to runner UID/GID 1001. An absent cache directory is created; existing
symlinks, nonempty unmounted caches and foreign mounts are refused. Repeated
preparation checks the source, filesystem type, size and options without mounting
a second filesystem. It does not change the workspace, service configuration,
workflow, test selection or coverage.

A fresh 3 vCPU / 6 GiB verification VM proved the 2 GiB mount, ownership, native
execution and repeat preparation. It was deleted before starting the full trial.
The preceding baked-image job used about 1.27 GB of package cache, including
804 MB of Yarn files and 469 MB of uv files, which fits the proposed cap. This is
an experimental profile, not an admission guarantee for arbitrary workflows.


The [RAM package cache trial](https://github.com/BreadGenie/frappe/actions/runs/37112715046)
passed all 708 tests with coverage in 699 seconds: containers 17 seconds, setup 166
seconds, tests 489 seconds and dispatch 53 seconds. Its
[paired GitHub run](https://github.com/BreadGenie/frappe/actions/runs/37112717413)
took 340 seconds (containers 20, setup 79, tests 234). Compared with the preceding
700-second baked-image run, this is effectively unchanged. The package RAM cache
is therefore left off in the dogfood profile; this trial does not establish a
useful performance improvement.


## Same-interpreter native and nested CPU probe

An unregistered one-vCPU guest and the Metal host ran the same Python 3.14.7
binary and standard library copied from the published image. Each probe used
three repetitions at scale 0.25, then the host/guest order was reversed. No CI
job was running. The temporary host interpreter copy was removed afterward.

| Probe | Native host, operations/s | Guest, operations/s |
| --- | ---: | ---: |
| Integer loop, first pass | 6,186,223 | 5,744,671 |
| Integer loop, reverse pass | 6,082,444 | 5,919,360 |
| Float loop, first pass | 15,508,970 | 14,856,433 |
| Float loop, reverse pass | 15,573,870 | 14,834,121 |
| getpid loop, first pass | 4,098,100 | 5,125,125 |
| getpid loop, reverse pass | 3,960,304 | 5,179,274 |

The guest was about 3–7% slower on the integer loop and 4–5% slower on the float
loop; its getpid loop was faster. These small synthetic probes do not reproduce
the full Frappe workload, and compare native versus nested execution on this
host, not this host versus GitHub's hardware. They do not establish a uniform
virtualization penalty large enough to explain the full test-phase gap.


## Bounded workspace in RAM

The separate `--workspace-tmpfs-mib 2048` profile mounts `/home/runner/work` as
`runway-workspace` before registration. Package caches remain on disk. It uses
the same ownership, executable mount options, requested-capacity-plus-2-GiB
memory admission and fresh-directory checks as the package cache profile.
Both profiles remain off by default, and enabling them together is rejected
before SSH and inside the guest. This keeps the experiment to one variable and
avoids reserving both mounts against the same 6 GiB guest.

A never-registered verification VM proved the 2 GiB mount, native execution,
runner ownership and repeat preparation; the package cache was not mounted.
That VM was deleted before the full workflow trial. The workspace cap includes
checkout, virtualenvs, node modules, runner temporary files and job artifacts.
Workflows requiring more space should retain the disk-backed default.


The [workspace RAM trial](https://github.com/BreadGenie/frappe/actions/runs/37113881319)
passed all 708 tests with coverage in 720 seconds: containers 18 seconds, setup
161 seconds, tests 482 seconds, coverage upload 35 seconds and dispatch 44
seconds. The [paired GitHub job](https://github.com/BreadGenie/frappe/actions/runs/37113883269)
took 508 seconds (containers 25, setup 92, tests 384, coverage upload 1).
The test phase was effectively unchanged from the 484-second baked-image trial.
Setup saved 10 seconds, but the complete job was 20 seconds slower, including a
longer coverage upload. This does not establish a useful overall gain; the
workspace RAM profile is also left off in the dogfood configuration.

| Profile, same source and 708 tests with coverage | Runway job | Paired GitHub job |
| --- | ---: | ---: |
| Baseline | 749 s | 512 s |
| Loopback registry mirror | 762 s | 345 s |
| Baked public service images | 700 s | 440 s |
| Baked images + RAM package cache | 699 s | 340 s |
| Baked images + RAM workspace | 720 s | 508 s |

These are individual trials, not a statistically established speedup. GitHub
reference times varied substantially. Retain the baked-image profile as the
measured candidate, keep both RAM flags off, and reproduce multiple full runs
before making broader performance claims. Overall GitHub timing parity has not
been reached.
