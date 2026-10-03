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
