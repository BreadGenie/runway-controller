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
Those two probes were sequential, single-file and run on an idle guest with no
job present. The download half still holds for large sequential fetches. The
fsync half never generalised: 200 fsyncs in 0.296 s is 1.48 ms each, already
5.2x the host's 285 µs, and it measures one file at a time rather than the many
small cold reads a test phase is made of. See
[what limits the test phase](#what-limits-the-test-phase).

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

The guest-side block measurements need root, because a cold pass is only cold
if the page cache was actually dropped. Without it the probe says
`page_cache_dropped: false` on both passes and reports no I/O share, rather than
presenting a warm read as a cold one:

```sh
python3 scripts/profile-frappe-run.py tree-read --tree /opt/hostedtoolcache/Python
python3 scripts/profile-frappe-run.py tree-read --tree /opt/hostedtoolcache/Python --threads 8
```

The tree creates, deletes and mounts nothing. It does evict the page cache of
the kernel it runs on, which is why it wants root and why that is not a harmless
thing for a shared machine. To compare the same bytes against a RAM-backed
path, point `--tree` at a tmpfs mount that is already set up. Repeating with
`--threads 1`, `4` and `8`, dropping the host's cache between runs, is what
separates a latency-bound path from a throughput-bound one.

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
virtualization penalty large enough to explain the full test-phase gap. They also
do not rule one out: a pure-compute probe cannot see the small cold reads that
the test phase is largely made of, which is measured separately below.


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
workspace RAM profile is also left off in the dogfood configuration. Why it was
a null result is in
[what limits the test phase](#what-limits-the-test-phase): this trial already had
the job's Python read path in RAM with 3 GB still available, and the phase was
never storage-bound to begin with.

These are individual trials, not a statistically established speedup. GitHub
reference times varied substantially. Retain the baked-image profile as the
measured candidate, keep both RAM flags off, and reproduce multiple full runs
before making broader performance claims. Overall GitHub timing parity has not
been reached.

Neither RAM profile should be retried in that form, and the reason is now
measured rather than guessed. Putting the package cache or the workspace in RAM
was aimed at storage latency, and the guest really does spend most of a cold
file read waiting on storage. But the test phase spends almost none of its time
waiting on storage: 0.05% iowait on average, 0% at the 95th percentile,
measured during the phase itself. The workspace trial already had the job's
Python read path in RAM with 3 GB still free. Both profiles moved the test phase
by about two seconds either way across five runs, which is the expected result
once the phase is known not to be storage-bound. What they traded away was
memory, and that trade was not free.

## Open blocker on the paired live trial

Attaching `scripts/profile-frappe-run.py sampler` to a real job is the
outstanding measurement, and the one that would show what the workload spends
its time on. The host and guest `vmstat` capture described in
[what limits the test phase](#what-limits-the-test-phase) already answers the
storage half of that; what is missing is per-process CPU and the MariaDB
`SHOW GLOBAL STATUS` counters, which is what would test the round-trip
hypothesis. Setting the sampler up got as far as a claimed pool and a fresh
scale set, and then stopped on the GitHub side.

`--action controller` reported `GitHub session unavailable; lease deadlines
remain enforced by Central`, then stopped when Central returned HTTP 508 on
ownership renewal, a query deadlock, handled by stopping rather than continuing
with stale ownership. The same status appears elsewhere in that pool's earlier
controller logs, on VM ensure and on delete, so it is not specific to renewal.
The controller reports that one generic message for any failure to open the
Actions message session, so the underlying status is not visible from its log.
What is established:

- The credential is not the problem. `--action lookup` succeeded against the
  same token for a scale set created seconds earlier, returned live statistics,
  and afterwards reported `scale set not found`, a message Runway only prints
  when the SDK call itself succeeded, so the token and the Actions service
  credential exchange both work.
- `--action delete` also succeeded.
- The failing call is message-session creation, `POST
  _apis/runtime/runnerscalesets/{id}/sessions`. Runway sets `RetryMax = 0` on
  the HTTP client for that call, so it is attempted once per loop and retried
  every three seconds; no status code was ever observed, because the controller
  does not log one.

Not yet ruled out: whether a scale set needs time before it will open a message
session, or whether the session endpoint rejects a repository-scoped scale set
created moments earlier. The scale set was removed and the pool returned to
Paused with `scale_set_id` restored, so nothing is left registered.

Recreating the scale set and waiting before starting the controller is the next
step, and it needs no new credentials. If it fails the same way, the error needs
to be surfaced from the controller rather than collapsed into one generic
health line, which is a change worth making in its own right: a failure that
stops all side effects should say what failed.

## What limits the test phase

Everything above ranks profiles. This section asks what actually limits the
`Run Tests` phase, and it revises an earlier conclusion in this document in the
process.

### The test phase is not I/O bound, and a live capture already showed it

Host and guest `vmstat`, plus host `iostat`, were captured over SSH during the
baseline Runway job and analysed with this repository's own `hostio`
subcommand. The guest capture is 376 samples at two-second intervals, about
752 s against the job's 749 s, ending when the guest stopped
answering, so it spans the job rather than an idle probe. This was done before, not after, the
question was asked; what is missing is the richer `sampler` JSONL, which adds
per-process CPU and MariaDB counters.

Over the last 237 samples, which fall inside the test phase:

| Guest, during the test phase | mean | p50 | p95 | max |
| --- | ---: | ---: | ---: | ---: |
| user | 26.18 | 26.00 | 51.00 | 65.00 |
| system | 8.18 | 9.00 | 14.00 | 20.00 |
| **idle** | **64.00** | **63.00** | 94.00 | 96.00 |
| **iowait** | **0.05** | **0.00** | **0.00** | 4.00 |
| steal | 1.56 | 1.00 | 3.00 | 5.00 |
| run queue | 1.45 | 1.00 | 3.00 | 6.00 |

Busy CPU is 36%, which on three vCPUs is about 1.08 cores, with a run queue of
1.45. Iowait is 0.05% on average and 0% at the 95th percentile: across the
whole 474 s window that is roughly a quarter of a second spent waiting on I/O.
Over the entire capture, iowait averages 0.93%, still about 7 s. The choice of
window moves the answer by more than twentyfold, so the window matters, but
neither choice gets near the 75.7–231.7 s the marginal term has to explain, and
neither can be the mechanism. Steal tops out at 5%.

So the test phase is not waiting on guest disk, not waiting on CPU, and barely
loses anything to the hypervisor. It is latency-bound: something outside those
three counters accounts for roughly two-thirds of the wall time while the
guest's cores sit idle.

This also settles what the two RAM profiles did and did not show. The workspace
profile mounted `/home/runner/work`, which contains the job's
`env/lib/python3.14/site-packages`, on a 2 GiB tmpfs, so the Python read path
was in RAM for that trial. Its own capture during the test phase recorded
3,020,528 kB `MemAvailable`, 1,594,880 kB `Shmem`, and 884,600,832 bytes free on
the mount: a RAM-backed read path with 3 GB still available and no memory
pressure. The test phase was 482 s against the baked image's 484 s. That is not
a null result waiting for an explanation; it is the expected result, and it
should not be read as evidence that the read path was cheap or that RAM was the
constraint. The package-cache profile is a weaker version of the same
experiment and likewise showed nothing.

### The guest's block path is genuinely poor, and still is not the limit

Measured on a never-registered VM booted from image `cffdaa3f24a245ca` at the
pool's own 3 vCPU / 6144 MiB / 10240 MiB shape, with no job and no controller
running; both probe VMs were deleted afterwards. The probe reads
`/opt/hostedtoolcache/Python`, which holds 8,345 `.py`, `.pyc` and `.so`
files totalling 267,925,688 bytes, the same read-mostly content class as a job's
`site-packages`. Host baseline on the ext4 filesystem over `/dev/vda1`: 9,638 4 KiB random reads/s
(mean 103.8 µs) and about 3,500 4 KiB durable writes/s (mean 285 µs).

| Measurement | Result |
| --- | ---: |
| Tree read, host and guest caches both dropped | 3.66–4.11 s, 439–493 µs/file |
| Tree read, guest cache dropped only | 1.89 s, 227 µs/file |
| Tree read, warm | 0.086–0.108 s, 10–13 µs/file |
| I/O share of a cold tree read | 95–97% |
| Sequential read, cold | 710–993 MiB/s |
| 4 KiB durable write | 438–449 ops/s, 2.23–2.28 ms |
| Same tree from a 1 GiB tmpfs | 1.42 s, so 2.69x faster |
| Tree read at 1 / 4 / 8 readers | 2,207 / 3,561 / 3,475 files/s |

Three measurements, and one inference that is not one.

A cold read of this tree is 95–97% I/O wait at 227–493 µs per file, against
10–13 µs warm. That is a 22x to 34x lever along this path, and it is the
number that makes the guest look bad on paper.

Durable 4 KiB writes cost 2.23–2.28 ms against the host's 285 µs, a factor
of 8.0, reproduced across both VMs.

Reader concurrency saturates: eight readers buy 1.575x over one, and four buy
1.614x. One sample per point, so treat the shape as indicative rather than a
curve.

The inference: why the block path behaves this way. metald hands Firecracker a
raw ZFS zvol as `{path_on_host, is_root_device: true, cache_type: "Writeback"}`
and sets no `io_engine`, so Firecracker falls back to its documented default of
Sync, one in-flight request per virtqueue, and no vhost-user socket, so
virtio-blk is serviced inside the Firecracker process. `grep` over the deployed
`/usr/bin/metald` finds no occurrence of `io_engine` or `vhost`, and
`metald.toml` exposes no drive-backend or device-tuning keys. Below that, the
`metal` ZFS pool's only vdev is the *file* `/root/disks/atlas.img` on ext4
over a virtio disk whose queue reports `rotational = 1`. That accounts for the
queue-depth observation and for roughly half the cold-read cost surviving a
host-cache drop (1.81 s of 3.55 s), but it is read off configuration that is
absent, not measured. None of it is reachable from this repository: Runway picks
pool size and guest preparation, and never configures the hypervisor.

None of this explains the gap, because the test phase does not spend its time
reading. It is recorded because it is real, because it will bound any future
I/O-heavy workflow on this host, and because a workload that is genuinely
read-bound on Runway should be told to expect it.

### Where that leaves the gap

Across the five logged pairs, the `Run Tests` step spans 484–492 s on Runway
(1.017x) and 234–388 s on GitHub (1.661x). That asymmetry is more useful than
either number. It is not explained by cache state, step set, image digest,
service digests or test count: all ten logs ran the same 16 steps, the same 123
classes, the same 708 tests, zero failures, identical action SHAs, the identical
`ubuntu-24.04 20260927.320.1` image, identical service digests, and three cache
hits each with no `Failed to restore` line. Within the five GitHub runs the
whole step and its first segment correlate at Pearson r = +0.997 (n=5,
indicative). Across platforms the step ratio and that segment's ratio correlate
at +0.927, held down by the RAM-cache pair, whose Runway import segment was
7.529 s against 5.358–6.001 s in the other four, which is a cold fresh VM
rather than a platform property.

So the test phase on both platforms is close to one machine-speed constant times
a fixed workload. Runway is a slower machine and a steady one; GitHub-hosted
runners are a variable machine that sometimes arrives faster than Runway and
sometimes slower. Runway's stability is worth something in its own terms, but it
does not make an absolute parity target well defined against a reference whose
own spread is 1.661x.

Normalising that constant out, the remaining platform difference is smaller than
the raw ratios suggest. Marginal cost per test is 0.6079–0.6161 s on Runway
(1.34% spread over five runs) against 0.2863–0.4828 on GitHub; with the constant
divided out the per-test multiplier is 1.245–1.305, median 1.268. About
97–103% of the gap is that marginal term; fixed per-class cost is at most
0.10 s per class and is *lower* on Runway in four of the five pairs. These come
from an out-of-tree analysis of the five pairs rather than from a documented
`profile-frappe-run.py` subcommand, so the numbers are not reproducible from
the tooling as it stands.

The shape of what is left: a workload that keeps about 1.1 of 3 cores busy,
shows no iowait, loses at most 5% to steal, and is still 1.27x slower per test
after machine speed is normalised out, while a pure-compute probe puts the
guest only 3–7% behind the host and a syscall probe ahead of it. That points at
per-operation latency rather than throughput, and the largest untested candidate
is MariaDB round-trips: the database runs in a container on tmpfs inside the
guest, so a query wait is neither guest iowait nor guest CPU, and it is the one
thing the existing captures cannot see. The network device is serviced in the
same userspace Firecracker process as the block device, with the same absent
vhost configuration, so it is exposed to the same class of overhead, but that is
an inference, and the round-trip cost has not been measured on either platform.

The measurement to take is therefore a MariaDB round-trip latency distribution,
guest against host and Runway against a GitHub-hosted runner, taken during a job.
That needs a live trial.

Two per-class results from the paired-run analysis are outliers rather than
platform properties and should not be carried into any comparison. `TestSMTP`,
ratio 15.76x, is not hermetic: the logs show it connecting to public SMTP servers
on port 587, five different Google addresses across the five GitHub runs and one
fixed non-Google address across the five Runway runs, never the smtp4dev service
the workflow starts. Its per-connect cost was 0.060–0.304 s on GitHub and
1.056–1.295 s on Runway. Separately, the workspace trial's coverage-upload step
took 35 s, of which about 28 s was a stall on a 172,005-byte artifact against
1.8 s for 172,030 bytes in another run.
