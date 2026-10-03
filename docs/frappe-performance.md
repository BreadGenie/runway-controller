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

Attaching `scripts/profile-frappe-run.py sampler` to a real job was the
outstanding measurement. The host and guest `vmstat` capture described in
[what limits the test phase](#what-limits-the-test-phase) already answered the
storage half of it; what was missing was per-process CPU and the MariaDB
`SHOW GLOBAL STATUS` counters, which is what would test the round-trip
hypothesis. That is now done, and
[the trial section](#the-clocksource-trial-and-the-round-trip-count) reports it.

Setting it up first failed, and the cause was not GitHub. `--action controller`
reported `GitHub session unavailable; lease deadlines remain enforced by
Central`, then stopped when Central returned HTTP 508 on ownership renewal, a
query deadlock, handled by stopping rather than continuing with stale ownership.
The same status appears elsewhere in that pool's earlier controller logs, on VM
ensure and on delete, so it is not specific to renewal.

What the investigation established, in the order it mattered:

- The credential was never the problem. `--action lookup` and `--action jit`
  both succeeded against the same token, the latter returning a runner ID, so the
  token and the Actions service credential exchange both work.
- A standalone probe of `Client.MessageSessionClient` against the same scale set
  opened a session with no error at all.
- The controller was in fact being pointed at a scale set that no longer existed.
  An operator helper used to set the pool's `scale_set_id` and status saved the
  document without committing it, so the writes never reached the database while
  the helper reported success from the in-memory document. The controller
  therefore read the previous `scale_set_id` and tried to open a session against
  a deleted scale set.
- Two things made this slow to see. A paused pool answers a claim with
  `enabled: false`, so a pool that was never activated is a quiet no-op rather
  than an error. And the controller collapses every message-session failure into
  one generic health line, so a configuration mistake presented as a GitHub
  outage.

The last point is a real defect worth fixing on its own terms and independent of
this investigation: a failure that stops all side effects should say what failed.
Surfacing the underlying error is what would have made this a two-minute
diagnosis. Reading a pool's configuration back through the API, rather than
trusting a helper's own report of what it wrote, is the other half.

The trial has since been run twice successfully, and the scale set has again
been removed and the pool returned to Paused with `scale_set_id` restored.

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

The measurement to take is therefore how many round trips the 708 tests make,
which would put a per-operation cost onto a per-test basis. The next section
measures the per-operation costs; counting the operations still needs a live
trial to close.

Two per-class results from the paired-run analysis are outliers rather than
platform properties and should not be carried into any comparison. `TestSMTP`,
ratio 15.76x, is not hermetic: the logs show it connecting to public SMTP servers
on port 587, five different Google addresses across the five GitHub runs and one
fixed non-Google address across the five Runway runs, never the smtp4dev service
the workflow starts. Its per-connect cost was 0.060–0.304 s on GitHub and
1.056–1.295 s on Runway. Separately, the workspace trial's coverage-upload step
took 35 s, of which about 28 s was a stall on a 172,005-byte artifact against
1.8 s for 172,030 bytes in another run.

## Per-operation costs and the clocksource

The per-operation costs left unattributed above are now measured, and they are
two things rather than one. A clock read costs about 4x what it costs on a
GitHub-hosted runner, and a loopback round trip costs about 3x, dominated by
waking a vCPU that has gone idle. The second is a property of the virtualization
boundary and Runway does not control it. The first is a clocksource choice in
the guest, and it is the only clean lever here: switching it takes clock reads
from 4.16x to 1.14x of a GitHub-hosted runner and moves nothing else measurably.

What this does not close is the attribution to the test phase. The costs are
measured; how many of each operation 708 tests perform is not, so the share of
the 92 s the gap is made of that these two account for is still an estimate. Both
statements above are microbenchmarks on an unloaded guest, and no end-to-end job
has been run with the clocksource change, so neither is a prediction about the
phase.

### What the per-operation costs actually are

`scripts/platform-probe.py` is the canonical tool for this. It is standard
library only, strictly a reader that opens one loopback listener on an ephemeral
port, and prints one JSON object per measurement. Each row below names the
statistic it reports, because they are not all the same and a reader should not
have to guess which:

```sh
python3 scripts/platform-probe.py                     # every section
python3 scripts/platform-probe.py --list-sections
python3 scripts/platform-probe.py --section clock --section network
```

The GitHub-hosted column came from the same file inlined into a dispatched
workflow on the `runway-dogfood` branch of `BreadGenie/frappe`. That workflow is
not on `main` here, so reproducing the GitHub-hosted column needs a dispatched
run in that repository.

Both sides ran the same interpreter, `/usr/bin/python3` 3.12.3, which the
probe's own identity line confirms on the guest. The guest was a
never-registered VM from image `cffdaa3f24a245ca` at the pool's own
3 vCPU / 6144 MiB / 10240 MiB shape on the Metal host, with no job and no
controller running; the GitHub-hosted runner reported 4 vCPU and 16 GiB. Guest
figures are the median of three whole runs, with the range over those three runs
in the cell. The clock rows report the fastest of three repetitions, the
interpreter rows the median of three, the sleep row the median of 40 samples,
and the round trip the median and p95 of 2,000 rounds.

| Measurement | GitHub-hosted | Runway guest | Ratio |
| --- | ---: | ---: | ---: |
| `time.monotonic()` | 49.4 ns/call | 200.5 ns/call (198.8–200.8) | 4.06x |
| `time.perf_counter()` | 49.0 ns/call | 199.7 ns/call (199.3–206.1) | 4.08x |
| `time.time()` | 48.4 ns/call | 205.0 ns/call (200.5–205.9) | 4.24x |
| `sleep(1 ms)` overshoot, median | 59.3 µs | 116.9 µs (104.1–117.1) | 1.97x |
| `interpreter_dispatch` | 51.38 ns/iter | 58.22 ns/iter (57.46–58.77) | 1.13x |
| `interpreter_allocate` | 258.79 ns/iter | 394.84 ns/iter (391.09–409.37) | 1.53x |
| `rtt_loopback`, median | 27.36 µs | 81.08 µs (80.65–90.16) | 2.96x |
| `rtt_loopback`, p95 | 33.02 µs | 109.23 µs (104.11–129.35) | 3.31x |

The interpreter rows say what the earlier probes in this document already said.
Against a GitHub-hosted runner, bytecode dispatch is 1.13x and an
allocation-heavy loop is 1.53x. Against the Metal host rather than against
GitHub's hardware, the same-interpreter comparison is 1.017x and the tight C loops
are 1.02–1.07x, which is the basis of
[the CPU probe section](#same-interpreter-native-and-nested-cpu-probe) and a
different question from the GitHub-hosted column here. Neither set is large
enough to be the phase.

One hypothesis is rejected outright. The image pins the official
`actions/python-versions` 3.14.7 tarball by SHA256
(`guest/prepare-frappe-image.sh`), and GitHub ships that same build, so the
interpreter build is not a platform difference here. An out-of-tree comparison
of the two builds found the pinned 3.14.7 has `USE_COMPUTED_GOTOS = 0` where a
stock 3.14.4 has it set, worth about 1.7x on bytecode dispatch. That is worth
knowing about the image, and it cancels out of a comparison where both platforms
run the same binary.

The clock rows are the outlier at 4.06x to 4.24x, and the `strace` count
explains why: in the guest, 470 `clock_gettime` syscalls for 20
`time.monotonic()` calls. The guest kernel logs `kvm-clock: Using msrs
4b564d01 and 4b564d00`, so its kvm-clock is MSR-backed with no pvclock page, the
guest vDSO has nothing to read in userspace, and every clock read in every
process is a full syscall. GitHub-hosted reports `tsc`; the guest reports
`kvm-clock`, with `tsc` in `available_clocksource`. The same count was not taken
on the GitHub-hosted side, so the two mechanisms are inferred from the guest's
syscall count and the two platforms' reported clocksource rather than measured
on both.

### Why the guest's clock reads cost four times as much

Switching the guest to `tsc` and rerunning the same probe, three whole runs
before and three after, in one session with no job present:

| Measurement | Guest, `kvm-clock` | Guest, `tsc` | GitHub-hosted | Before | After |
| --- | ---: | ---: | ---: | ---: | ---: |
| `clock_cost_monotonic` | 205.5 ns (200.7–206.4) | 56.5 ns (56.2–62.0) | 49.4 ns | 4.16x | 1.14x |
| `clock_cost_perf_counter` | 202.0 ns (200.8–206.6) | 56.1 ns (55.7–63.0) | 49.0 ns | 4.12x | 1.14x |
| `clock_cost_time` | 208.9 ns (201.2–209.2) | 57.9 ns (57.5–63.1) | 48.4 ns | 4.32x | 1.20x |
| `interpreter_dispatch` | 61.48 ns (57.67–61.70) | 59.09 ns (58.24–59.66) | 51.38 ns | 1.20x | 1.15x |
| `interpreter_allocate` | 407.12 ns (401.3–468.2) | 399.93 ns (398.8–417.1) | 258.79 ns | 1.57x | 1.55x |
| `rtt_loopback`, median | 82.18 µs (81.97–100.56) | 89.51 µs (69.68–91.96) | 27.36 µs | 3.00x | 3.27x |
| `sleep_overshoot_1ms` | 107.5 µs (96.5–117.5) | 107.1 µs (97.0–108.4) | 59.3 µs | 1.81x | 1.81x |

After the switch the `strace` count for the same 20 calls is zero, and all three
`after` captures report `current: tsc`.

Nothing else moved beyond its own noise. The rep-to-rep spread on allocation,
loopback round trip and sleep overshoot is 4.6–25% of the median within a single
configuration, taking the range over the median of three repetitions, and the
before-to-after movement on those three rows is 1.8%, 8.9% and 0.4%; bytecode
dispatch moved 3.9% against a 6.6% spread. This is a clocksource result, not a
broad speedup, and it should be quoted as one.

The residual 1.14x is not the vDSO either: with zero syscalls the reads are
already userspace `rdtsc`. What it is, is not established. The obvious candidate
does not survive the out-of-tree measurement, which put this guest's TSC-backed
read at 54.07 ns against the Metal host's pvclock-backed 58.60 ns, and 20.14 ns
against 26.68 ns in C. With a working TSC the guest's clock read is faster than a
host's, so the cost of the read itself is not available as an explanation for a
deficit against a GitHub-hosted runner, and the remainder is carried forward
unaccounted.

The write is root only. `current_clocksource` is mode `0644 root:root` and the
runner's uid 1001 gets `EACCES`, so the switch has to run as root or be set when
the image is built. It is not a knob a job can reach.

### The round trip is vCPU placement, not networking

The probe's own round trip runs its client and echo server as two threads in one
process, so it cannot show placement. It was replaced, out of tree, by a C client
and server in separate processes, one byte-identical static binary on both
machines (each harness statically linked and re-verified by md5 after transfer),
8-byte payload, 20,000 timed iterations per run after 2,000 warmup, six runs
each. Raw captures are kept outside the repository and the aggregates are
published here.

| Bare loopback TCP, 8 B, six runs | Metal host | Runway guest |
| --- | ---: | ---: |
| median of six run medians | 20.57 µs (17.44–23.26, 29.1%) | 77.10 µs (73.50–77.67, 5.4%) |
| p95 of six run p95s | 24.34 µs (18.14–24.80, 28.4%) | 103.21 µs (96.04–108.98, 12.6%) |

That is 3.75x on the median and 4.24x on p95, and it agrees with the 81.08 µs
the in-process probe measured in its own session.

That 3.75x is not a constant, because both machines are bimodal at the level of
whole runs. A second independent six-run set, recorded as run medians only
rather than as retained samples, gave host values of either 17.5 or 24.0 µs and
guest values of either 9.5 or 75.1 µs. Compared mode
to mode, the guest's slow mode is 3.13x the host's and its fast mode is 1.84x
faster than the host's, so a three-repetition median on this measurement lands
in one mode or the other depending on the run. It should be read as "the
guest's slow mode is about 3x the host's", not as a multiplier.

Pinning the two ends decides which mode a run lands in:

| Placement, 8 B, three runs each | Metal host | Runway guest |
| --- | ---: | ---: |
| Both ends on vCPU 0 | 10.12 µs | 9.40 µs |
| Client on vCPU 1 | 17.42 µs | 84.59 µs |
| Premium for using a second vCPU | +7.31 µs | +75.19 µs |
| Busy-poll at both ends, 8 B | 7.66 µs | 6.35 µs |
| Busy-poll at both ends, 64 KiB | 30.94 µs | 25.12 µs |

Confined to one vCPU the guest is faster than the host, and under busy-polling
it is faster still at both payload sizes. All of the penalty is the wake.

It is not the network stack either. Repeating the pinned measurement with no
networking at all, an `AF_UNIX` socketpair and a pipe between two processes:

| Transport, three runs each | Host, one vCPU | Host, two vCPUs | Guest, one vCPU | Guest, two vCPUs |
| --- | ---: | ---: | ---: | ---: |
| `SOCK_SEQPACKET` | 4.52 µs | 18.84 µs | 4.32 µs | 57.08 µs |
| `SOCK_DGRAM` | 4.75 µs | 12.25 µs | 4.33 µs | 57.14 µs |
| Pipe | 6.73 µs | 9.87 µs | 4.90 µs | 51.95 µs |

The guest's cross-vCPU premium is 52.8 µs on both socket types and 47.1 µs on
the pipe, with 0.4–0.7% spread over three runs on the socket transports, against
3–14 µs on the host. TCP, netfilter, offloads, MTU and qdisc are all out of the
picture by then and the gap is still there.

Ruled out by measurement: GRO and GSO offload (`ethtool -k lo` is identical on
both, MTU already 65536, `lo` is `noqueue`); payload size, flat from 1 B to
8 KiB; netfilter, where the host carries 52 `iptables-save` lines against the
guest's 34 and so does more work per packet; RPS, `busy_poll` and CPU isolation,
identical on both; idle-CPU burn, measured at 0.04 cores for an idle guest;
qdisc and congestion control. The missing pvclock page is not implicated either:
it is worth about 254 ns of extra clock read per round-trip sample, 0.33% of the
slow mode, and the same harness put the guest's clock read at 159 ns against the
host's 32 ns, so the guest carries a 5x worse clock and a faster loopback. That
ruling rests on the measurement and on the mechanism rather than on a direct A/B,
because the guest kernel was never rebuilt with a pvclock-capable configuration
to confirm it.

Not settled: how the roughly 53 µs splits between guest IPI resume, KVM exit and
re-entry, and host scheduling of the target vCPU thread. KVM exit counts were
not instrumented, so the split is unknown. What is established is that the cost
lives at the virtualization boundary, because it survives with networking
removed and disappears when both ends share one vCPU.

The shape that matters for this workload is the published port, because the
workflow publishes MariaDB's port. The probe's own container case returned
`never accepted a connection` in all three guest runs and produced no number, so
the published-port path was measured with the C harness against a `mariadb:11.8`
container instead: 113.99, 191.61 and 136.79 µs median over three runs, against
76.26, 76.67 and 76.64 µs for the container's own bridge IP. The bridge is free,
at the bare-loopback slow mode, and `docker-proxy` adds a second userspace hop
worth +40 to +115 µs on top, which is two more sleep/wake transitions. That hop
was measured in the guest only, so it is not established how much of it is the
guest and how much is `docker-proxy` anywhere. A per-query cost in a Runway guest
should therefore be expected at 115–190 µs median, though that is a container
round trip through a published port and not a measured MariaDB query, and
neither this group nor the spinner rows below has a retained capture.

### What Runway can change, and what it cannot

| Lever | Measured | Cost |
| --- | --- | --- |
| Guest clocksource, as root or at image build | 4.16x to 1.14x on clock reads | Nothing else moved; no job measured |
| Pin both ends of a loopback exchange to one vCPU | 84.59 to 9.40 µs, 9.0x | Serialises the peers; a workload decision, not a guest default |
| `busy_poll` plus `SO_BUSY_POLL` | 76.6 to 6.35 µs, 12x | Needs a client-side change; burns a whole vCPU |
| A vCPU keeper spinner | 57.5 to 40.8 µs, -29%, no retained capture | p99.9 stalls of 9.4–9.6 ms |
| Fewer vCPUs per guest | 9.40 µs at one vCPU | Only pays at one vCPU, and gives up the parallelism the pool is sized for |

Rebuilding the guest kernel is not on that list and should not be attempted.
`HZ=250` against the host's 1000 and `PREEMPT_NONE` cannot produce this: the
same binary on the same vCPU is faster in the guest, and there is no 4 ms
quantization anywhere in the distribution (minimum 26.7 µs, p95 74–79 µs). The
guest is in fact better on `NO_HZ_IDLE`.

The round-trip cost is not reachable from this repository. Runway picks pool
size and guest preparation and never configures the hypervisor, the same
position
[the block path section](#the-guests-block-path-is-genuinely-poor-and-still-is-not-the-limit)
records for Firecracker's `Sync` io engine and the absent vhost configuration.
What would help is a request to Atlas and Metal rather than a Runway change:
either a vCPU wake path that does not require the host to reschedule a vCPU
thread for every idle-to-runnable transition, or a documented host CPU pinning
for a guest's vCPUs, so that the placement which costs 75 µs is a known quantity
rather than whatever the scheduler chose.

### What is still not established

- The probe's own loopback measurement puts the client and the echo server in
  one process, so both platforms share a GIL. The two-process C measurement is
  the one to rely on.
- That C measurement has no GitHub-hosted counterpart. Getting one needs a
  dispatched workflow, so the only comparison available is the guest against the
  Metal host.
- The probe ran `/usr/bin/python3` 3.12.3, which is not the 3.14.7 toolcache
  interpreter the job uses, so the two interpreter rows describe the platform's
  Python rather than the workload's own.
- No end-to-end job has been run with `clocksource=tsc`. The effect on the
  708-test phase is unmeasured, and the aggregate effect of the clock and the
  round trip on job time remains an estimate rather than a trial.
- The cost per round trip is now measured; the number of round trips the 708
  tests actually make is not. The next section counts them, from a live job
  rather than from `sampler`, and closes both this and the clocksource question.

## The clocksource trial and the round-trip count

The two items the previous section left open are now both measured: a job has run
end to end with `tsc` selected in the guest, and the number of database round trips
that 708 tests make has been counted. Neither closes the gap, and the second
narrows it by less than half.

The opt-in was set in the controller's `transport_command` with
`--prefer-guest-tsc`, so guest `prepare` selects `tsc` rather than leaving the
kernel's default `kvm-clock`. Two jobs ran on the dogfood pool and both passed all
708 tests with coverage. The clock was confirmed applied inside the job rather than
assumed from the controller's configuration: the counter capture records the
guest's own `current_clocksource` at every sample and reads `cs=tsc` at all 179 of
the samples inside the second job's test phase. The counters below come from that
same capture, so the confirmed clocksource and the round-trip count describe one
window of one job rather than two separate claims.

### The opt-in was applied and bought nothing

| Trial run | `Run Tests` step | Initialize containers | Environment Setup | Tests passed |
| --- | ---: | ---: | ---: | ---: |
| Run 1 | 488.25 s | 16.40 s | 173.08 s | 708 of 708 |
| Run 2 | 493.63 s | 18.08 s | 183.42 s | 708 of 708 |

Step durations are from the job logs, measured the same way as the five earlier
Runway runs so the comparison is like for like. Those five, which did not set the
clocksource, took 485.53, 490.45, 482.86, 486.80 and 480.05 s: a mean of 485.14 s
and a sample standard deviation of 3.94 s. The two trial runs average 490.94 s.
That is **+5.80 s, and the trial shows no improvement**.

Two runs cannot resolve anything much smaller than that. With a sample standard
deviation of 3.94 s across the five prior runs, the standard error of a two-run
mean is 2.79 s, so the difference between two such means carries about 3.94 s. The
trial is therefore silent about an effect at that scale, and the +5.80 s it does
show sits 1.47 standard deviations from the earlier mean, which is unremarkable.

The figures elsewhere in this document that give 484 to 492 s for the same runs
are GitHub API step durations rather than log-derived ones, and sit about 1.5 s
higher. Nothing turns on the choice, but the comparison above uses one measure
throughout.

The reason is now available, and it is not that the microbenchmark was wrong. The
microbenchmark did not predict this and could not have. The switch moves a clock
read from 205.5 ns to 56.5 ns, a saving of 149.0 ns. At one clock read per database
round trip and the 474 round trips a test makes, that is 70.6 µs per test, or 0.05 s
across all 708 tests. For the change to move the phase by the roughly 4 s this trial can
resolve, the workload would have to spend about 80 clock reads per round trip. It
evidently does not, so the workload is not clock-bound on its critical path and the
4.16x that motivated the opt-in was real and irrelevant to the phase.

### 474 round trips per test

The capture is a read-only poll inside the guest for the life of one job: MariaDB's
`SHOW GLOBAL STATUS` for a fixed list of variables, plus `MemAvailable` from
`/proc/meminfo` and `pgmajfault` from `/proc/vmstat`, sampled every three seconds.
It is 224 sample blocks in total. The window used here is the 179-sample run that
brackets run 2's test phase, 19:15:17 to 19:23:26 UTC, 489 s wide. MariaDB's own
`Uptime` counter moves 195 to 684 across the same two samples, so the window is
489 s by the database's clock as well as by the capture's. The step itself runs
19:15:15.343 to 19:23:28.977, which puts the bracket 1.7 s late at the start and
3.0 s early at the end.

| Counter | Delta over the 489 s | Per test |
| --- | ---: | ---: |
| `Questions` | 335,599 statements | 474.0 statements |
| `Com_select` | 216,486 statements | 305.8 statements |
| `Com_insert` | 44,764 statements | 63.2 statements |
| `Com_update` | 9,438 statements | 13.3 statements |
| `Com_delete` | 15,554 statements | 22.0 statements |
| `Com_commit` | 7,191 statements | 10.2 statements |
| `Innodb_data_reads` | 302 pages | 0.4 pages |
| `Innodb_data_writes` | 2,154 pages | 3.0 pages |
| `Bytes_sent` | 485,419,944 bytes | 685,621 bytes |

The two InnoDB counters are data-page reads and writes. That is **474 database
round trips per test and 686 per second**, and `Bytes_sent` alone is 485 MB out of
the database during the phase, about 0.99 MB/s. The five captured `Com_*` counters
sum to 293,433, so 42,166 statements, 12.6% of the total, fall outside them: the
capture's variable list did not include `Com_rollback`, `Com_connect`,
`Com_change_db` or the administrative statements. `Questions` is the complete count
for the phase; the breakdown beneath it is not.

The same capture answers the storage question again, from inside the same window,
and it agrees with the retraction above:

| Guest, over the same 489 s | Value |
| --- | ---: |
| `pgmajfault` | +299 faults, or 0.61 faults/s |
| `MemAvailable` | 4.04 GB to 4.68 GB |

299 major page faults while running 708 tests is 0.42 per test, and `MemAvailable`
never left a 4.04–4.68 GB band. The phase was not waiting on storage and was not
short of memory either, which is why the tmpfs profile and the two RAM profiles
could not pay, and it is why the database round trips were the next thing to count.

### What the round trips account for, and what is left

The marginal term to explain is the 1.268x per-test multiplier on a 484 s
no-clocksource baseline: 0.268 of 484 s is 130 s, and 0.268 of 492 s is 132 s, so
the denominator does not depend on which prior run is taken as the baseline. It
sits inside the 75.7–231.7 s the previous section already gives for the same term.
Multiplying 335,599 round trips by each of the three per-round-trip penalties
measured there:

| Assumed penalty per round trip | Guest | GitHub-hosted | Implied extra time | Share of 130 s |
| --- | ---: | ---: | ---: | ---: |
| Bare loopback | 81.08 µs | 27.36 µs | 18.0 s | 13.9% |
| Published port, low end | 115 µs | 27.36 µs | 29.4 s | 22.6% |
| Published port, high end | 190 µs | 27.36 µs | 54.6 s | 42.0% |

Each guest cost above less 27.36 µs is a penalty: 53.72 µs, 87.64 µs and 162.64 µs.
Multiplying each by the 335,599 round trips gives the last two columns.

So the round trips are a real and measurable contributor worth between 14% and 42%
of the marginal term on their own, roughly a seventh to two fifths. The
GitHub-hosted container round trip was never measured, only its loopback path,
which is why the range is this wide and why the low end is the better-supported
figure. It also means the two published-port rows subtract a loopback figure from a
container figure, so if GitHub's own published-port path costs more than its
loopback, those two rows overstate the penalty rather than understate it.

The remainder is the point. Taking off the 54.6 s and the 18.0 s ends leaves 75 s
to 112 s, 58% to 86% of the marginal term, unaccounted for by round trips. It sits
in the other measured per-operation gaps, principally allocation at 1.53x and
bytecode dispatch at 1.15x, plus whatever is still unmeasured. This section does
not break that remainder down further, because nothing in this capture can: the
counters count round trips, not the work the client does between them.

### What this section does not establish

- One job, one window. The counters are from a single run on a single pool, and the
  two bracketing samples are 3 s apart at the median, so the phase boundaries are
  good to about 3 s out of 489 s and the counter window slightly overshoots the
  486.2 s of test execution inside it.
- `Questions` counts statements, not client-server round trips. A statement that
  returns rows and one that does not cost different amounts of waiting, so 474 per
  test is an upper bound on round trips and a lower bound on their cost. The two
  errors run in opposite directions, so their product is a bound in neither.
- Two trial runs are the only ones with the clocksource set, and Runway's own
  test-phase spread is 4.00 s across five prior runs. Nothing under roughly 4 s is
  resolvable from this comparison.
- The clock-save estimate above assumes the clock reads happen inside the database
  round trips. Coverage tracing, logging and the HTTP calls to `smtp4dev` read
  clocks too, and none of them are counted here.
- The capture was streamed off the guest to the Metal host rather than written
  inside it, because the controller reclaims the VM within seconds of the job ending
  and the first attempt, which wrote to a file in the guest, lost its data that way.
  Raw captures are kept outside the repository; only aggregates are published here.

The opt-in works, is free, and is not a speedup, because this workload is not
clock-bound on its critical path. The round-trip count is now measured at 474 per
test and accounts for between a seventh and two fifths of the marginal gap on its
own. The largest remaining unattributed share sits in allocation and bytecode
dispatch, and neither is reachable from this repository: the same pinned Python
build runs on both platforms, so it cancels out of any comparison, and Runway
configures neither the allocator nor the interpreter. Parity has not been reached
and the gap is not closed.
