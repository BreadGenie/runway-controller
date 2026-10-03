#!/usr/bin/env python3
"""Profile and compare Frappe "Server Tests" CI logs on the Runway pilot.

Subcommands
-----------
compare   Parse two GitHub Actions job logs (a Runway run and the matched
          GitHub-hosted reference) and rank the per-class / per-test time gap.
steps     Print the coarse phase breakdown (setup / test / teardown) of one log.
sampler   Read-only operational sampler: host + guest CPU/io/steal, top
          processes, and MariaDB commit/fsync counters. Writes JSONL.
fsync-probe
          Single-file fsync/commit microbenchmark used to prove or disprove
          "storage sync latency" as the cause of the test-phase gap. Runs
          entirely inside a scratch directory; touches no service config.
tree-read  Cold-versus-warm read cost across a whole directory tree, which is
          what a single-file fsync probe cannot see: the test phase reads very
          many small files and only the first pass is slow. Creates, deletes
          and mounts nothing, but evicts the page cache of the kernel it runs
          on, which is why it wants root.

Nothing here mutates host, guest, VM or FC service state.  Every command is a
pure reader except `sampler` and `fsync-probe`, which only write into a
directory you name with --out, and `tree-read`, which evicts this kernel's page
cache and nothing else.  That last one is not harmless on a shared machine: it
affects every process, not just this one.  The other two write paths require
that directory to be private to the invoking user: it is created 0700 when
missing and refused when it is shared or a symlink.  Output files are always
*new*: created O_CREAT|O_EXCL|O_NOFOLLOW at 0600, so anything already at that
path is left untouched and the run is refused.  Point --out at a path that is
not taken.

Stdlib only.
"""

from __future__ import annotations

import argparse
import errno
import json
import os
import re
import stat
import socket
import statistics
import sys
import time
from collections import OrderedDict, defaultdict
from concurrent.futures import ThreadPoolExecutor
from dataclasses import dataclass, field
from datetime import datetime, timezone
from typing import Iterator, Sequence

# ---------------------------------------------------------------------------
# Log line parsing
# ---------------------------------------------------------------------------

# Log downloads in this project carry ANSI escapes in two shapes: real ESC
# bytes and the literalised "\x1b" -> "^[" form.  Strip both.
ANSI_RE = re.compile(r"(?:\x1b|\^\[)\[[0-9;?]*[A-Za-z]")

# "<job>\t<step>\t<ISO8601 with 7 fractional digits>Z <message>"
TS_RE = re.compile(
    r"^(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d+)Z[ \t]?(.*)$", re.S
)

# frappe/testing/result.py emits, for each test, exactly
#     f"   {click.style(status, fg=color)} {test_method}"
# with three statuses and no others: " ✔ " (pass), " ✖ " (fail/error), " = "
# (skipped).  test_method is the bare method name.  Tests slower than
# SLOW_TEST_THRESHOLD (2s) get a " (12.345s)" suffix appended, which is an
# exact, self-reported per-test duration.
#
# The class keeps the sibling check/cross marks the runner may pick between
# (" ✔ ", " ✘ ", " ✖ ", " ✗ ", " × ", " ✓ ") plus the ASCII "=" skip marker:
# all are Unicode symbols that cannot plausibly start an ordinary log line, so
# tolerating them costs nothing and keeps a version that styles its output with
# a different mark from silently dropping every failing test.
#
# What must stay out is ASCII *letters*.  "x" and "u" were briefly accepted here
# and they are a real hazard: an ordinary wrapped log line such as
# "x 3 checks skipped" then parsed as a test result and pushed results_seen past
# the runner's own "Tests:" count, which is the number every report and the
# regression suite are calibrated against.  See ResultRegexTests.
STATUS_GLYPHS = "\u2714\u2718\u2716\u2717\u00d7\u2713="
RESULT_RE = re.compile(
    r"^\s*[" + STATUS_GLYPHS + r"]\s+([A-Za-z_][A-Za-z0-9_]*)\s*"
    r"(?:\((\d+(?:\.\d+)?)s\))?\s*$"
)
# A test class header is a bare dotted path emitted by
# frappe/testing/result.py:71  (unittest.util.strclass).
HEADER_RE = re.compile(r"^[A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z_][A-Za-z0-9_]*)+$")
SUMMARY_RE = re.compile(r"^Tests:\s*(\d+),\s*Failing:\s*(\d+),\s*Errors:\s*(\d+)")
HOOK_RE = re.compile(r"^(Before|After) Test\s*\(([\d.]+)s\)")

# Lines that are pure runtime noise: they can land between two test results and
# would otherwise steal time from whichever test we attribute it to.  Also
# captures Frappe's replayed class-setup output ("  ▹ ", "  ▸ ").
NOISE_RE = re.compile(
    r"^(?:"
    # "<path>:<line>: SomeWarning: msg" -- covers real and relative paths.
    r"\S+:\d+:(?: |$)"
    r"|\s+with\b"  # continuation of a warning
    r"|Traceback \(most recent call last\)"
    r"|During handling of the above exception"
    r"|[A-Za-z_.]+(?:Error|Exception|Warning)(?::|$)"
    r"|##\[(group|endgroup|command|debug|warning|error)\]"
    r"|\s*[▹▸]"  # Frappe's buffered class-setup echo
    r"|\s*$"
    r")"
)


@dataclass(frozen=True)
class Line:
    ts: datetime
    job: str
    step: str
    text: str


def parse_timestamped_lines(path: str) -> Iterator[Line]:
    """Yield every timestamped log line from a GitHub Actions log download."""
    with open(path, encoding="utf-8", errors="replace") as handle:
        for raw in handle:
            raw = raw.rstrip("\n")
            parts = raw.split("\t")
            if len(parts) < 3:
                continue
            job, step, tail = parts[0], parts[1], "\t".join(parts[2:])
            match = TS_RE.match(tail.lstrip("\ufeff"))
            if not match:
                continue
            stamp = match.group(1)
            # GitHub emits 7 fractional digits; datetime only takes 6.
            if len(stamp) - stamp.index(".") - 1 > 6:
                stamp = stamp[: stamp.index(".") + 7]
            try:
                ts = datetime.strptime(stamp, "%Y-%m-%dT%H:%M:%S.%f").replace(
                    tzinfo=timezone.utc
                )
            except ValueError:
                continue
            yield Line(ts=ts, job=job, step=step, text=ANSI_RE.sub("", match.group(2)))


# ---------------------------------------------------------------------------
# Test-event extraction
# ---------------------------------------------------------------------------


@dataclass
class TestCase:
    name: str
    cls: str
    ts: datetime
    # Lower bound: time since the previously printed line of any kind.
    dur_min: float
    # Upper bound: time since the previous *significant* event, so noise lines
    # emitted while the test ran are charged to it instead of vanishing.
    dur_max: float
    # Exact self-reported duration, printed by the runner for tests over
    # SLOW_TEST_THRESHOLD (2s).  None for faster tests.
    reported_s: float | None = None

    @property
    def best(self) -> float:
        """Most trustworthy duration available for this test."""
        return self.reported_s if self.reported_s is not None else self.dur_max


@dataclass
class ClassSpan:
    cls: str
    start: datetime
    end: datetime
    tests: list[TestCase] = field(default_factory=list)
    setup_hooks: list[tuple[str, float]] = field(default_factory=list)

    @property
    def total(self) -> float:
        return (self.end - self.start).total_seconds()

    @property
    def test_time(self) -> float:
        return sum(t.best for t in self.tests)

    @property
    def reported_test_time(self) -> float:
        return sum(t.reported_s for t in self.tests if t.reported_s is not None)

    @property
    def overhead(self) -> float:
        return self.total - self.test_time


@dataclass
class RunProfile:
    path: str
    steps: "OrderedDict[tuple[str, str], list[datetime]]" = field(default_factory=OrderedDict)
    classes: "OrderedDict[str, ClassSpan]" = field(default_factory=OrderedDict)
    orphan_tests: list[TestCase] = field(default_factory=list)
    tests_reported: int | None = None
    results_seen: int = 0
    first_result: datetime | None = None
    last_result: datetime | None = None
    hooks: list[tuple[str, float, datetime]] = field(default_factory=list)
    coverage_warnings: list[tuple[datetime, str]] = field(default_factory=list)

    # -- derived ----------------------------------------------------------
    @property
    def wall(self) -> float:
        if not self.steps:
            return 0.0
        first = min(v[0] for v in self.steps.values())
        last = max(v[1] for v in self.steps.values())
        return (last - first).total_seconds()

    @property
    def test_wall(self) -> float:
        if self.first_result is None or self.last_result is None:
            return 0.0
        return (self.last_result - self.first_result).total_seconds()

    def rank_classes(self) -> list[ClassSpan]:
        return sorted(self.classes.values(), key=lambda c: c.total, reverse=True)


def _new_span(profile: RunProfile, cls: str, start: datetime) -> ClassSpan:
    span = ClassSpan(cls=cls, start=start, end=start)
    profile.classes[cls] = span
    return span


def profile_log(path: str, step_filter: str | None = None) -> RunProfile:
    """Turn a job log into a per-class / per-test timing profile.

    `step_filter` matches the step name exactly by default.  Substring
    matching would also pull in "Post <step>", which double-counts the step.
    """
    profile = RunProfile(path=path)
    current: ClassSpan | None = None
    prev_any: datetime | None = None
    prev_sig: datetime | None = None

    for line in parse_timestamped_lines(path):
        if step_filter is not None and line.step != step_filter:
            continue
        bounds = profile.steps.setdefault((line.job, line.step), [line.ts, line.ts])
        bounds[0] = min(bounds[0], line.ts)
        bounds[1] = max(bounds[1], line.ts)

        text = line.text.strip()
        result = RESULT_RE.match(line.text)
        hook = HOOK_RE.match(text)
        summary = SUMMARY_RE.match(text)

        if hook:
            profile.hooks.append((hook.group(1), float(hook.group(2)), line.ts))
            if current is not None:
                current.setup_hooks.append((hook.group(1), float(hook.group(2))))
            prev_any = prev_sig = line.ts
            continue
        if summary:
            profile.tests_reported = int(summary.group(1))
            prev_any = prev_sig = line.ts
            continue
        if "coverage" in text.lower() and (
            "CoverageWarning" in text or "combine" in text.lower()
        ):
            profile.coverage_warnings.append((line.ts, text[:160]))

        if result:
            base_min = prev_any or line.ts
            base_max = prev_sig or line.ts
            case = TestCase(
                name=result.group(1),
                cls=current.cls if current else "<unknown>",
                ts=line.ts,
                dur_min=max(0.0, (line.ts - base_min).total_seconds()),
                dur_max=max(0.0, (line.ts - base_max).total_seconds()),
                reported_s=float(result.group(2)) if result.group(2) else None,
            )
            profile.results_seen += 1
            if profile.first_result is None:
                profile.first_result = line.ts
            profile.last_result = line.ts
            if current is None:
                profile.orphan_tests.append(case)
            else:
                current.tests.append(case)
                current.end = line.ts
            prev_any = prev_sig = line.ts
            continue

        if HEADER_RE.match(text) and "." in text:
            current = _new_span(profile, text, line.ts)
            prev_any = prev_sig = line.ts
            continue

        if not NOISE_RE.match(line.text):
            prev_sig = line.ts
        prev_any = line.ts

    return profile


# ---------------------------------------------------------------------------
# Comparison
# ---------------------------------------------------------------------------


def _fmt(sec: float) -> str:
    return f"{sec:8.2f}"


def _pct(values: list[float], q: float) -> float:
    ordered = sorted(values)
    return ordered[min(len(ordered) - 1, int(q * len(ordered)))]


def compare(baseline: RunProfile, candidate: RunProfile, top: int) -> str:
    out: list[str] = []
    w = out.append
    w("# Frappe test-phase comparison")
    w("")
    w(f"reference (GitHub-hosted): {baseline.path}")
    w(f"candidate  (Runway)      : {candidate.path}")
    w("")
    w("## Totals")
    w("")
    w("| metric | reference | candidate | delta | ratio |")
    w("| --- | ---: | ---: | ---: | ---: |")
    rows = [
        ("tests reported", baseline.tests_reported, candidate.tests_reported),
        ("results parsed", baseline.results_seen, candidate.results_seen),
        ("classes", len(baseline.classes), len(candidate.classes)),
        ("test wall (s)", baseline.test_wall, candidate.test_wall),
        ("selected log span (s)", baseline.wall, candidate.wall),
    ]
    for label, a, b in rows:
        if a is None or b is None:
            w(f"| {label} | {a} | {b} | n/a | n/a |")
            continue
        ratio = (b / a) if a else float("nan")
        w(f"| {label} | {a} | {b} | {b - a:+.2f} | {ratio:.2f}x |")
    w("")

    # ---- per-class ------------------------------------------------------
    bmap = {c.cls: c for c in baseline.classes.values()}
    cmap = {c.cls: c for c in candidate.classes.values()}
    shared = [k for k in cmap if k in bmap]
    only_c = [k for k in cmap if k not in bmap]
    only_b = [k for k in bmap if k not in cmap]

    deltas = sorted(
        ((k, cmap[k].total - bmap[k].total, cmap[k].total, bmap[k].total)
         for k in shared),
        key=lambda r: r[1],
        reverse=True,
    )

    w(f"## Per-class gap (candidate - reference), {len(shared)} classes in both")
    w("")
    w("| delta (s) | cand (s) | ref (s) | ratio | cand overhead (s) | tests | class |")
    w("| ---: | ---: | ---: | ---: | ---: | ---: | --- |")
    for key, delta, ctot, btot in deltas[:top]:
        span = cmap[key]
        ratio = ctot / btot if btot else float("nan")
        w(
            f"| {delta:+.2f} | {ctot:.2f} | {btot:.2f} | {ratio:.2f}x | "
            f"{span.overhead:.2f} | {len(span.tests)} | `{key}` |"
        )
    w("")
    if deltas:
        neg = [d for d in deltas if d[1] < 0]
        w(
            f"classes slower: {sum(1 for d in deltas if d[1] > 0)} / {len(deltas)}"
            f"   faster: {len(neg)}   "
            f"net delta: {sum(d[1] for d in deltas):+.2f}s over matched classes"
        )
        w("")
        # Uniformity of the per-class ratio is the key diagnostic: a single
        # pathological hot spot gives one huge outlier, whereas a flat
        # distribution across every class points at a systemic per-operation
        # cost (durable commit latency, or CPU) rather than one slow test.
        ratios = [c / b for _, _, c, b in deltas if b >= 0.5]
        if ratios:
            w("### Is the gap concentrated or uniform?")
            w("")
            w(f"ratio over {len(ratios)} classes with >=0.5s of reference time: "
              f"min {min(ratios):.2f}x  p25 {_pct(ratios, .25):.2f}x  "
              f"median {_pct(ratios, .5):.2f}x  p75 {_pct(ratios, .75):.2f}x  "
              f"p90 {_pct(ratios, .90):.2f}x  max {max(ratios):.2f}x")
            w("")
            buckets = [0, 0, 0, 0, 0, 0]
            labels = ["<1.00x", "1.00-1.15x", "1.15-1.30x", "1.30-1.50x",
                      "1.50-2.00x", ">2.00x"]
            for r in ratios:
                if r < 1.0:
                    buckets[0] += 1
                elif r < 1.15:
                    buckets[1] += 1
                elif r < 1.30:
                    buckets[2] += 1
                elif r < 1.50:
                    buckets[3] += 1
                elif r < 2.00:
                    buckets[4] += 1
                else:
                    buckets[5] += 1
            w("| ratio band | classes | share |")
            w("| --- | ---: | ---: |")
            for label, count in zip(labels, buckets):
                w(f"| {label} | {count} | {100 * count / len(ratios):.1f}% |")
            w("")
            share = (deltas[0][1] / sum(d[1] for d in deltas)
                     if sum(d[1] for d in deltas) else float("nan"))
            w(f"**top class holds {100 * share:.1f}% of the entire gap** "
              f"(`{deltas[0][0]}`, {deltas[0][1]:+.2f}s).**")
            w("")
        w(f"top {min(10, len(deltas))} classes by absolute share of the candidate test wall:")
        w("")
        w("| cand (s) | % of cand test wall | ref (s) | class |")
        w("| ---: | ---: | ---: | --- |")
        wall = candidate.test_wall or 1.0
        for span in candidate.rank_classes()[:top]:
            w(
                f"| {span.total:.2f} | {100 * span.total / wall:.1f}% | "
                f"{bmap[span.cls].total if span.cls in bmap else float('nan'):.2f} | "
                f"`{span.cls}` |"
            )
        w("")
    if only_c:
        w(f"classes only in candidate ({len(only_c)}): "
          + ", ".join(f"`{k}`" for k in only_c[:10]))
        w("")
    if only_b:
        w(f"classes only in reference ({len(only_b)}): "
          + ", ".join(f"`{k}`" for k in only_b[:10]))
        w("")

    # ---- per-test -------------------------------------------------------
    def test_index(prof: RunProfile) -> dict[str, TestCase]:
        idx: dict[str, TestCase] = {}
        for span in prof.classes.values():
            for case in span.tests:
                idx[f"{span.cls}::{case.name}"] = case
        return idx

    bidx = test_index(baseline)
    cidx = test_index(candidate)
    keys = [k for k in cidx if k in bidx]
    rows_t = sorted(
        ((k, cidx[k].dur_max - bidx[k].dur_max, cidx[k], bidx[k]) for k in keys),
        key=lambda r: r[1],
        reverse=True,
    )
    w(f"## Per-test gap (dur_max), {len(keys)} tests matched by class::name")
    w("")
    w("| delta (s) | cand (s) | ref (s) | cand min (s) | test |")
    w("| ---: | ---: | ---: | ---: | --- |")
    for key, delta, c, b in rows_t[:top]:
        w(f"| {delta:+.2f} | {c.dur_max:.2f} | {b.dur_max:.2f} | {c.dur_min:.2f} | `{key}` |")
    w("")

    # ---- within-class concentration ------------------------------------
    w("## Where the candidate spends its own test wall")
    w("")
    cand_totals = sorted(
        ((s.total, s) for s in candidate.classes.values()), reverse=True, key=lambda r: r[0]
    )
    cum = 0.0
    w("| cum % | class (s) | class |")
    w("| ---: | ---: | --- |")
    for tot, span in cand_totals:
        cum += tot
        if 100 * cum / wall > 92 and cum - tot > 0.15 * wall:
            break
        w(f"| {100 * cum / wall:.1f}% | {tot:.2f} | `{span.cls}` |")
    w("")
    w("## Setup / teardown hooks (self-reported by the runner)")
    w("")
    w("| phase | reference | candidate |")
    w("| --- | ---: | ---: |")
    for kind in ("Before", "After"):
        rb = [v for k, v, _ in baseline.hooks if k == kind]
        rc = [v for k, v, _ in candidate.hooks if k == kind]
        w(f"| {kind} Test | {sum(rb):.2f} ({len(rb)}) | {sum(rc):.2f} ({len(rc)}) |")
    w("")
    return "\n".join(out)


def steps_report(prof: RunProfile) -> str:
    out = ["# Phase breakdown: " + prof.path, "",
           "| job | step | start | end | seconds |", "| --- | --- | --- | --- | ---: |"]
    for (job, step), (a, b) in prof.steps.items():
        out.append(f"| {job} | {step} | {a.isoformat()} | {b.isoformat()} | "
                   f"{(b - a).total_seconds():.2f} |")
    out += ["", f"test wall (first->last result): {prof.test_wall:.2f}s",
            f"results parsed: {prof.results_seen}, reported: {prof.tests_reported}"]
    return "\n".join(out)


# ---------------------------------------------------------------------------
# Sampler-log analysis (vmstat / iostat captures taken during a real job)
# ---------------------------------------------------------------------------


def parse_vmstat_log(path: str) -> list[dict]:
    """Parse `vmstat -w`-style output into dicts.  No timestamps: rows are
    ordered, so callers slice by index once they know when sampling started."""
    rows: list[dict] = []
    with open(path, encoding="utf-8", errors="replace") as handle:
        lines = handle.readlines()
    for line in lines:
        parts = line.split()
        if len(parts) < 17 or not parts[0].isdigit():
            continue
        try:
            # vmstat -w emits 18 columns; plain vmstat emits 16.  Accept either
            # and zip only as many as we actually supplied.
            values = (float(p) for p in parts[: len(VMSTAT_COLS)])
            rows.append(dict(zip(VMSTAT_COLS, values)))
        except ValueError:
            continue
    return rows


def parse_iostat_log(path: str) -> dict[str, list[dict]]:
    """Parse `iostat -x` output.  Each block repeats its own `Device` header,
    so the header row is used as the schema rather than hardcoded offsets."""
    header: list[str] | None = None
    devices: dict[str, list[dict]] = defaultdict(list)
    with open(path, encoding="utf-8", errors="replace") as handle:
        lines = handle.readlines()
    for line in lines:
        parts = line.split()
        if not parts:
            continue
        if parts[0] == "Device":
            header = parts
            continue
        if header and len(parts) == len(header) and not parts[0][0].isdigit():
            row = dict(zip(header[1:], parts[1:]))
            devices[parts[0]].append(row)
    return devices


def _stats(values: list[float]) -> str:
    """mean / p50 / p95 / max in markdown-table cell form."""
    if not values:
        return "n/a | n/a | n/a | n/a"
    ordered = sorted(values)
    return (f"{statistics.fmean(ordered):8.2f} | {statistics.median(ordered):8.2f} | "
            f"{ordered[int(0.95 * (len(ordered) - 1))]:9.2f} | {ordered[-1]:9.2f}")


def _stat_pairs(values: list[float]) -> tuple[float, float]:
    if not values:
        return float("nan"), float("nan")
    ordered = sorted(values)
    return statistics.fmean(ordered), ordered[int(0.95 * (len(ordered) - 1))]


def hostio_report(args: argparse.Namespace) -> str:
    """Summarise sampler captures.

    Provenance matters when reading the output.  `--host-*` files are captured on
    the metal node and describe the *host* kernel; `--guest-*` files are captured
    from inside the VM and describe the *guest* kernel.  `iostat -x` output is
    host-only -- it is not available from inside the guest -- so device names
    there (`vda`, `zd16`) are the host's view of the VM's virtio disk and of the
    ZFS zvol backing it, never the guest's own storage stack.  Each table below
    is labelled with the machine that produced it so the two are not mixed.
    """
    out: list[str] = ["# Sampler analysis", ""]

    for label, source, path in (
            ("HOST", "metal node, host kernel", args.host_vmstat),
            ("GUEST", "inside the VM, guest kernel", args.guest_vmstat)):
        if not path or not os.path.exists(path):
            continue
        rows = parse_vmstat_log(path)
        if not rows:
            continue
        lo = max(0, args.skip)
        hi = len(rows) if args.count <= 0 else min(len(rows), lo + args.count)
        window = rows[lo:hi]
        note = ("whole capture" if args.skip == 0 and args.count <= 0
                else f"rows [{lo}:{hi}]")
        out += [f"## {label} vmstat - source: {source}", "",
                f"file: {path}", "",
                f"samples={len(rows)}  analysed: {note} ({len(window)} samples)", "",
                "| field | mean | p50 | p95 | max |",
                "| --- | ---: | ---: | ---: | ---: |"]
        for key in ("us", "sy", "id", "wa", "st", "bo", "bi", "r", "b", "cs"):
            if key in window[0]:
                out.append(f"| {key} | " + _stats([r[key] for r in window]) + " |")
        busy = [100 - r["id"] for r in window]
        steal_m, steal_p95 = _stat_pairs([r["st"] for r in window])
        wait_m, wait_p95 = _stat_pairs([r["wa"] for r in window])
        out += ["",
                f"- CPU busy: mean {statistics.fmean(busy):.1f}%",
                f"- **steal**: mean {steal_m:.2f}%, p95 {steal_p95:.2f}%, "
                f"max {max(r['st'] for r in window):.2f}%",
                f"- iowait: mean {wait_m:.2f}%, p95 {wait_p95:.2f}%",
                ""]

    if args.iostat and os.path.exists(args.iostat):
        devices = parse_iostat_log(args.iostat)
        out += [f"## HOST iostat - source: metal node, host kernel only", "",
                f"file: {args.iostat}", "",
                "Devices are host-side. `vda`/`vdb` are the VM's virtio disks as",
                "seen by the host; `zd*` are ZFS zvols on the host. None of these",
                "are visible from inside the guest, and MariaDB's data directory",
                "is tmpfs inside the guest, so it appears on neither.", ""]
        for dev, rows in devices.items():
            if len(rows) < 2:
                continue
            out += [f"### HOST device {dev} (n={len(rows)})", "",
                    "| metric | mean | p50 | p95 | max |",
                    "| --- | ---: | ---: | ---: | ---: |"]
            for key in ("w/s", "wkB/s", "w_await", "r/s", "rkB/s", "r_await",
                        "f/s", "f_await", "aqu-sz", "%util"):
                if key not in rows[0]:
                    continue
                vals = []
                for row in rows:
                    try:
                        vals.append(float(row[key]))
                    except ValueError:
                        pass
                if vals:
                    out.append(f"| {key} | " + _stats(vals) + " |")
            out.append("")
    return "\n".join(out)


# ---------------------------------------------------------------------------
# Operational sampler (read-only)
# ---------------------------------------------------------------------------

VMSTAT_COLS = ["r", "b", "swpd", "free", "buff", "cache", "si", "so", "bi", "bo",
               "in", "cs", "us", "sy", "id", "wa", "st", "gu"]
PROC_KEYS = ["utime", "stime", "state", "rss_pages"]
# MariaDB counters that expose commit/fsync pressure.
MARIADB_VARS = [
    "Innodb_data_fsyncs", "Innodb_data_writes", "Innodb_os_log_fsyncs",
    "Innodb_os_log_writes", "Innodb_buffer_pool_pages_dirty",
    "Innodb_row_lock_waits", "Innodb_row_lock_time",
    "Innodb_data_fsyncs_since_last_checkpoint" ,
]


def _sched_ids() -> list[int]:
    """Online CPU ids.  Falls back to os.cpu_count() because
    /sys/devices/system/cpu is often not mounted inside containers."""
    try:
        ids = sorted(int(n) for n in os.listdir("/sys/devices/system/cpu")
                     if n.isdigit())
        if ids:
            return ids
    except OSError:
        pass
    return list(range(os.cpu_count() or 1))


def _read_ps_once() -> list[dict]:
    """Cheap process snapshot from /proc (no ps, no fork storms).

    One malformed `/proc/<pid>/stat` must not kill a sampler that has hours
    left to run, so the field conversions are guarded too.
    """
    out = []
    for entry in os.listdir("/proc"):
        if not entry.isdigit():
            continue
        pid = int(entry)
        try:
            with open(f"/proc/{pid}/stat") as fh:
                data = fh.read()
        except OSError:
            continue
        try:
            tail = data[data.rindex(")") + 1:].split()
            with open(f"/proc/{pid}/comm") as fh:
                comm = fh.read().strip()
        except (OSError, ValueError):
            continue
        # tail[0] is state; fields 12/13 (1-based 14/15) are utime/stime.
        if len(tail) < 13:
            continue
        try:
            utime, stime = int(tail[11]), int(tail[12])
        except ValueError:
            continue
        out.append({
            "pid": pid,
            "comm": comm,
            "state": tail[0],
            "utime": utime,
            "stime": stime,
        })
    return out


def _diff_procs(before: list[dict], after: list[dict], interval: float,
                hz: int) -> list[dict]:
    bmap = {p["pid"]: p for p in before}
    amap = {p["pid"]: p for p in after}
    rows = []
    for pid, a in amap.items():
        b = bmap.get(pid)
        if b is None:
            continue
        d = (a["utime"] - b["utime"] + a["stime"] - b["stime"]) / hz
        if d <= 0.01:
            continue
        rows.append({"pid": pid, "comm": a["comm"], "cpu_s": round(d, 3),
                     "pct_one_core": round(100 * d / interval, 1)})
    rows.sort(key=lambda r: r["cpu_s"], reverse=True)
    return rows[:15]


def _private_dir(path: str) -> str:
    """Create or validate the directory that will hold `path`.

    A capture is only trustworthy if the directory it sits in is ours alone, so
    an existing parent has to clear all three of: a real directory (not a
    symlink, whose target could be swapped between the check and the open),
    private to the owner, and owned by the current euid.  A group/world
    accessible directory makes the capture readable by every other account on
    the metal node, and one belonging to somebody else is theirs to replace --
    either defeats the point of writing one.  Both are refused rather than
    quietly accepted, which includes the shared default `/tmp/runway-parity`
    until its operator makes it 0700 and owns it.

    Only the immediate parent is judged.  Ancestors are deliberately not:
    /tmp itself is 1777 by design, and its sticky bit already stops anyone but
    the owner from renaming or removing the private directory we hold inside it.
    A missing parent is created 0700, which satisfies all three by construction.
    """
    parent = os.path.dirname(os.path.abspath(path)) or "."
    try:
        info = os.lstat(parent)
    except FileNotFoundError:
        os.makedirs(parent, mode=0o700, exist_ok=True)
        # makedirs' mode is masked by the umask, and a umask that strips owner
        # bits would leave a capture directory nobody can write to next run.
        # We own this directory -- we just made it -- so pin it outright.
        os.chmod(parent, 0o700)
        info = os.lstat(parent)
    if stat.S_ISLNK(info.st_mode):
        raise OSError(errno.ELOOP,
                      f"output directory {parent} is a symlink; point --out at "
                      f"a real directory", parent)
    if not stat.S_ISDIR(info.st_mode):
        raise NotADirectoryError(errno.ENOTDIR,
                                 f"output directory {parent} is not a directory",
                                 parent)
    if info.st_mode & 0o077:
        raise PermissionError(
            errno.EACCES,
            f"output directory {parent} is mode {info.st_mode & 0o777:04o} and "
            f"is group/world accessible; run `chmod 700 {parent}` or point "
            f"--out somewhere private", parent)
    if info.st_uid != os.geteuid():
        raise PermissionError(
            errno.EACCES,
            f"output directory {parent} is owned by uid {info.st_uid}, not by "
            f"this process (euid {os.geteuid()}); point --out at a directory "
            f"you own", parent)
    return parent


def _open_private_fd(path: str, flags: int = os.O_WRONLY, mode: int = 0o600) -> int:
    """Create `path` as a brand-new private file.  Never an existing one.

    New captures only.  An existing entry is never opened, never truncated and
    never reused, whatever it looks like: `open(path, "w")` at a predictable
    name is how a capture path becomes an arbitrary-overwrite primitive, and
    even proving that a 0600 file is ours does not prove it was a previous
    *capture* rather than some other private file we have no business
    replacing.  So the caller has to name a path that does not exist yet, and
    O_CREAT|O_EXCL|O_NOFOLLOW turns anything already sitting there -- symlink,
    hardlink, fifo, or a perfectly ordinary file -- into a FileExistsError with
    that entry left exactly as it was.

    The parent is checked first (`_private_dir`): a name we cannot keep to
    ourselves is not a name we should be creating files under.
    """
    _private_dir(path)
    try:
        fd = os.open(path, flags | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, mode)
    except FileExistsError:
        raise FileExistsError(
            errno.EEXIST,
            f"output path {path} already exists; this tool only creates new "
            f"captures, so point --out (or --dir) at a path that is not taken",
            path) from None
    # os.open's mode is masked by the umask, so pin it explicitly: the
    # permissions are the other half of what makes the path safe to create in.
    os.fchmod(fd, mode)
    return fd


def _open_private(path: str, mode: int = 0o600):
    """Text-mode `_open_private_fd`; the sampler's and reports' output path."""
    return os.fdopen(_open_private_fd(path, os.O_WRONLY, mode), "w",
                     encoding="utf-8")


def sampler(args: argparse.Namespace) -> int:
    """Sample CPU/io/steal and process CPU time until --seconds elapses."""
    try:
        hz = os.sysconf("SC_CLK_TCK")
    except (ValueError, OSError):
        hz = 100
    deadline = time.monotonic() + args.seconds
    with _open_private(args.out) as sink:
        # Bracket the /proc snapshots, not the sleep.  The CPU delta below is
        # measured across the two snapshots, so the divisor has to be the wall
        # time between them -- which also covers the previous sample's own
        # /proc reads and its MariaDB query (up to its own 10s timeout).
        # Dividing by the requested sleep instead overstates pct_one_core by
        # however long a sample body took.
        t_prev = time.monotonic()
        prev_procs = _read_ps_once()
        while True:
            remaining = deadline - time.monotonic()
            sleep_for = max(0.2, min(args.interval, remaining))
            time.sleep(sleep_for)
            t_now = time.monotonic()
            procs = _read_ps_once()
            interval = t_now - t_prev
            record = {
                "t": datetime.now(timezone.utc).isoformat(),
                "monotonic": round(t_now, 3),
                "interval_s": round(interval, 3),
                "loadavg": os.getloadavg(),
                "meminfo": _meminfo(),
                "vmstat": _vmstat(),
                "procs": _diff_procs(prev_procs, procs, interval, hz),
                "mariadb": _mariadb_status(args.mariadb_socket,
                                           args.mariadb_defaults_file),
            }
            sink.write(json.dumps(record) + "\n")
            sink.flush()
            prev_procs, t_prev = procs, t_now
            if t_now >= deadline:
                break
    return 0


def _meminfo() -> dict:
    wanted = {"MemTotal", "MemAvailable", "Dirty", "Writeback"}
    out = {}
    try:
        with open("/proc/meminfo") as fh:
            for line in fh:
                k, _, rest = line.partition(":")
                if k in wanted:
                    out[k] = int(rest.split()[0])
    except OSError:
        pass
    return out


def _vmstat() -> dict:
    try:
        with open("/proc/vmstat") as fh:
            raw = fh.read().splitlines()
    except OSError:
        return {}
    interesting = ("pgmajfault", "pswpin", "pswpout", "pgpgin", "pgpgout",
                   "pgfault", "nr_dirty", "nr_writeback", "some_full")
    return {k: int(v) for k, v in (l.split()[:2] for l in raw) if k in interesting
            and v.isdigit()}


def _mariadb_status(socket_path: str | None, defaults_file: str | None = None) -> dict:
    """Read SHOW GLOBAL STATUS via the mariadb client, if one is reachable.

    Read-only.  Never starts, stops or reconfigures a server.

    Credentials are never placed in argv: the command line of any process is
    world-readable through /proc/<pid>/cmdline for as long as it lives, and
    this runs as root on the metal node, once per sample, for hours.  Because
    the connection is already forced over `--protocol=socket`, the client's own
    unix-socket auth is normally all that is needed and `defaults_file` stays
    None.  Where a password really is required, pass the *path* of a 0600 option
    file in `defaults_file`; the client reads it itself, so the contents never
    appear in a process listing.  A readable option file is refused rather than
    used.
    """
    if not socket_path:
        return {}
    if defaults_file:
        try:
            mode = os.stat(defaults_file).st_mode & 0o777
        except OSError as exc:
            return {"error": f"defaults file: {type(exc).__name__}"}
        if mode & 0o077:
            return {"error": f"defaults file mode {mode:04o} is group/world accessible"}
    import shutil
    import subprocess
    client = shutil.which("mariadb") or shutil.which("mysql")
    if not client:
        return {"error": "no mariadb client"}
    # The option file must come first when supplied: that is the configuration
    # the client reads, so no ambient ~/.my.cnf is consulted and nothing later
    # on the command line can redirect the connection.  Without one,
    # --no-defaults says the same about the ambient files.
    cmd = [client, f"--defaults-file={defaults_file}" if defaults_file
           else "--no-defaults"]
    cmd += ["--protocol=socket", f"--socket={socket_path}", "-NBe",
            "SHOW GLOBAL STATUS WHERE Variable_name IN (%s)"
            % ",".join("'%s'" % v for v in MARIADB_VARS)]
    try:
        done = subprocess.run(cmd, capture_output=True, text=True, timeout=10)
    except (OSError, subprocess.SubprocessError) as exc:
        return {"error": type(exc).__name__}
    if done.returncode != 0:
        return {"error": done.stderr.strip()[:120]}
    out = {}
    for row in done.stdout.splitlines():
        parts = row.split("\t")
        if len(parts) == 2 and parts[1].lstrip("-").isdigit():
            out[parts[0]] = int(parts[1])
    return out


def cpu_probe(args: argparse.Namespace) -> int:
    """Single-core CPU throughput probe.

    The test phase is latency-serial (~1 busy core of 3), so a uniform
    multiplicative slowdown across every test class points at per-core speed
    or per-operation latency rather than throughput or storage bandwidth.
    This measures single-core integer and float throughput plus syscall
    latency so those two causes can be told apart.  Pure computation: no I/O,
    no services, no allocation beyond a fixed list.
    """
    def integer_ops(n: int) -> int:
        acc = 0
        for i in range(n):
            acc = (acc * 1103515245 + 12345 + i) & 0xFFFFFFFF
        return acc

    def float_ops(n: int) -> float:
        acc = 0.5
        x = 1.0000001
        for _ in range(n):
            acc = acc * x + 0.25
            acc -= 0.25
        return acc

    def syscalls(n: int) -> int:
        import os as _os
        total = 0
        for i in range(n):
            try:
                total += _os.getpid()
            except OSError:
                pass
            total ^= i
        return total

    scale = args.scale
    report = {
        "scale": scale,
        "cpuinfo": _cpu_mhz(),
        "online_cpus": len(_sched_ids()),
        "python": sys.version.split()[0],
        "jit": _jit_status(),
    }
    workloads = (
        ("integer_ops_per_s", integer_ops, max(1, int(20_000_000 * scale))),
        ("float_ops_per_s", float_ops, max(1, int(20_000_000 * scale))),
        ("getpid_calls_per_s", syscalls, max(1, int(200_000 * scale))),
    )
    for name, fn, iters in workloads:
        best = 0.0
        for _ in range(args.repeats):
            t0 = time.perf_counter()
            fn(iters)
            elapsed = time.perf_counter() - t0
            if elapsed > 0:
                best = max(best, iters / elapsed)
        report[name] = round(best, 1)
        report.setdefault("iterations", {})[name] = iters
    print(json.dumps(report, indent=2))
    return 0


def _jit_status() -> dict:
    """PEP 744 JIT availability, via the real 3.14 `sys._jit` API.

    `is_available()` False means the interpreter was not *built* with the JIT
    (official actions/python-versions builds are not), so there is nothing to
    enable.  True + `is_enabled()` False means it is compiled in and merely
    switched off.  Older interpreters expose neither symbol.
    """
    out: dict = {}
    jit = getattr(sys, "_jit", None)
    if jit is None:
        out["api"] = "absent (interpreter predates PEP 744)"
        out["sysconfig"] = _jit_config()
        return out
    out["api"] = "sys._jit"
    out["is_available"] = jit.is_available()
    out["is_enabled"] = jit.is_enabled() if jit.is_available() else None
    out["sysconfig"] = _jit_config()
    return out


def _cpu_mhz() -> dict:
    out: dict = {}
    try:
        with open("/proc/cpuinfo") as fh:
            for line in fh:
                if line.startswith("model name"):
                    out.setdefault("model", line.split(":", 1)[1].strip())
                if line.startswith("cpu MHz"):
                    out.setdefault("mhz_sample", float(line.split(":", 1)[1]))
                    break
    except OSError:
        pass
    return out


def _jit_config() -> str:
    """Whether this interpreter was *built* with the PEP 744 JIT.

    Distinguishes "JIT compiled in but switched off" (fixable with
    PYTHON_JIT=1) from "JIT absent entirely" (nothing to enable).  The
    official actions/python-versions builds are not JIT builds.
    """
    import sysconfig
    for name in ("Py_JIT", "Py_ENABLE_JIT"):
        value = sysconfig.get_config_var(name)
        if value is not None:
            return f"{name}={value}"
    args = sysconfig.get_config_var("CONFIG_ARGS") or ""
    for token in args.split():
        if "jit" in token.lower():
            return f"CONFIG_ARGS {token}"
    return "no JIT in sysconfig (not a JIT build)"


# ---------------------------------------------------------------------------
# fsync microbenchmark
# ---------------------------------------------------------------------------


def _drop_page_cache() -> bool:
    """Try to evict this kernel's page cache.  Report whether it happened.

    Returns False rather than raising when the write is not permitted, because
    the caller still wants the pass -- it just has to know the numbers are warm
    and must not be reported as cold.
    """
    try:
        with open("/proc/sys/vm/drop_caches", "w") as fh:
            fh.write("3")
    except OSError:
        return False
    return True


def _tree_files(root: str, suffixes: Sequence[str]) -> list[str]:
    """Every file under `root` whose name ends with one of `suffixes`.

    Sorted so two runs of the same tree read the same files in the same order.
    """
    found = []
    for directory, _dirs, names in os.walk(root):
        for name in names:
            if name.endswith(tuple(suffixes)):
                found.append(os.path.join(directory, name))
    found.sort()
    return found


def _read_files(paths: Sequence[str]) -> tuple[float, int]:
    """Sequentially open and read every file in `paths`.

    Returns (wall seconds, bytes read).  Unreadable files are counted as read
    nothing rather than aborting the pass: a permission-denied entry should not
    turn a whole measurement into an error, but it does mean the byte total is
    a floor, not an exact figure.
    """
    start = time.monotonic()
    total = 0
    for path in paths:
        try:
            with open(path, "rb", buffering=0) as fh:
                while True:
                    chunk = fh.read(1 << 18)
                    if not chunk:
                        break
                    total += len(chunk)
        except OSError:
            continue
    return time.monotonic() - start, total


def tree_read_probe(args: argparse.Namespace) -> int:
    """Measure cold-versus-warm read cost across a whole directory tree.

    A single-file fsync microbenchmark cannot see what the Frappe test phase
    actually does: it reads very many small files, spread across a large tree,
    and only the first pass is slow.  This walks a tree (by default the baked
    `actions/setup-python` tool cache, the same read-mostly content class as a
    job's `site-packages`) once with the page cache dropped and once without,
    and reports how much of the cold pass was I/O wait.

    Creates, deletes and mounts nothing.  It does evict the page cache of the
    kernel it runs on, which affects every process on that machine and is why
    this wants root and should not be run casually on a shared host; to compare
    the same tree against a RAM-backed path, point `--tree` at an existing
    tmpfs mount the operator has already set up.

    Cache dropping is best-effort and is reported honestly: each pass carries
    `page_cache_dropped`, and the cold-versus-warm split is emitted only when
    the cold pass really was cold.  A run without permission to drop the cache
    reports two indistinguishable `false` values rather than inventing an I/O
    share out of noise.

    `hostname` is recorded because the same tree can exist on the host and in
    the guest, and a number without knowing which kernel produced it is not
    worth much.  Dropping the *other* kernel's cache is an out-of-band operator
    step this cannot perform or observe; pass `--host-cache-dropped` to record
    that it was done.
    """
    root = os.path.abspath(args.tree)
    if not os.path.isdir(root):
        raise SystemExit(f"--tree is not a directory: {root}")
    try:
        os.listdir(root)
    except OSError as exc:
        raise SystemExit(f"--tree is not readable: {root} ({type(exc).__name__})")
    if args.threads < 1:
        raise SystemExit("--threads must be at least 1")

    paths = _tree_files(root, args.suffix)
    if not paths:
        raise SystemExit(f"--tree contains no matching files: {root}")
    size = 0
    for path in paths:
        try:
            size += os.path.getsize(path)
        except OSError:
            continue

    passes: list[dict[str, object]] = []
    chunks = [paths[i::args.threads] for i in range(args.threads)]
    for label in ("cold", "warm"):
        dropped = _drop_page_cache() if label == "cold" else False
        if args.threads == 1:
            wall, nbytes = _read_files(paths)
        else:
            start = time.monotonic()
            with ThreadPoolExecutor(max_workers=args.threads) as pool:
                nbytes = sum(pool.map(lambda c: _read_files(c)[1], chunks))
            wall = time.monotonic() - start
        passes.append({
            "label": label,
            "page_cache_dropped": dropped,
            "wall_s": round(wall, 3),
            "wall_exact": wall,
            "bytes_read": nbytes,
            "files_per_s": round(len(paths) / wall, 1) if wall else None,
            "mib_per_s": round(nbytes / wall / 2**20, 1) if wall else None,
            "us_per_file": round(wall / len(paths) * 1e6, 1),
        })

    report: dict[str, object] = {
        "hostname": socket.gethostname(),
        "tree": root,
        "suffixes": list(args.suffix),
        "files": len(paths),
        "bytes": size,
        "threads": args.threads,
        "host_cache_dropped": args.host_cache_dropped,
        "passes": passes,
    }

    cold, warm = passes
    # Derived from the unrounded walls, not the rounded ones already in the
    # report: a sub-millisecond pass prints "wall_s": 0.0 next to a
    # files_per_s computed from the real value, and mixing the two produces
    # ratios that disagree with the numbers printed beside them.
    if cold["page_cache_dropped"]:
        report["io_seconds"] = round(cold["wall_exact"] - warm["wall_exact"], 3)
        if cold["wall_exact"] and warm["wall_exact"]:
            report["io_share_of_cold_pass"] = round(
                (cold["wall_exact"] - warm["wall_exact"]) / cold["wall_exact"], 3)
            report["warm_speedup"] = round(
                cold["wall_exact"] / warm["wall_exact"], 1)
    for entry in passes:
        del entry["wall_exact"]

    print(json.dumps(report, indent=2))
    return 0


def fsync_probe(args: argparse.Namespace) -> int:
    """Measure durable-commit latency for a scratch directory.

    Writes a file, fsyncs it, repeats.  Writes only under --dir (default: a
    fresh mkdtemp), which must be private to this user: it is created 0700 when
    absent and refused when shared or symlinked.  probe.bin is only ever created,
    so a second run against the same --dir is refused rather than overwriting
    the first one's numbers; use a fresh directory, or the default.  Deletes its
    own temp files on success.  Never touches ZFS pools, datasets, VM configs,
    or any service.
    """
    target = args.dir
    if target:
        created = False
    else:
        import tempfile
        target = tempfile.mkdtemp(prefix="fsync-probe-")
        created = True

    # Same private-output path as the sampler: probe.bin is a raw capture, and
    # --dir is validated (and created 0700) rather than assumed.
    fd = _open_private_fd(os.path.join(target, "probe.bin"), os.O_RDWR)
    payload = os.urandom(args.bytes)
    try:
        # Warm the page cache and allocator.
        for _ in range(5):
            os.pwrite(fd, payload, 0)
            os.fsync(fd)

        lat = []
        total = args.iters
        t0 = time.monotonic()
        for i in range(total):
            start = time.monotonic()
            os.pwrite(fd, payload, (i % 16) * args.bytes)
            os.fdatasync(fd)
            lat.append((time.monotonic() - start) * 1000.0)
        wall = time.monotonic() - t0

        sorted_lat = sorted(lat)
        report = {
            "dir": target,
            "bytes": args.bytes,
            "iters": total,
            "wall_s": round(wall, 3),
            "ops_per_s": round(total / wall, 1) if wall else None,
            "latency_ms": {
                "min": round(sorted_lat[0], 3),
                "p50": round(statistics.median(sorted_lat), 3),
                "p90": round(sorted_lat[int(0.9 * (total - 1))], 3),
                "p99": round(sorted_lat[int(0.99 * (total - 1))], 3),
                "max": round(sorted_lat[-1], 3),
                "mean": round(statistics.fmean(sorted_lat), 3),
            },
        }
        print(json.dumps(report, indent=2))
    finally:
        os.close(fd)
        if created and not args.keep:
            import shutil
            shutil.rmtree(target, ignore_errors=True)
    return 0


# ---------------------------------------------------------------------------
# CLI
# ---------------------------------------------------------------------------


def main(argv: Sequence[str] | None = None) -> int:
    ap = argparse.ArgumentParser(description=__doc__,
                                 formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = ap.add_subparsers(dest="cmd", required=True)

    c = sub.add_parser("compare", help="rank per-class/per-test gaps between two logs")
    c.add_argument("reference", help="GitHub-hosted job log (baseline)")
    c.add_argument("candidate", help="Runway job log")
    c.add_argument("--top", type=int, default=40)
    c.add_argument("--step", default="Run Tests", help="step substring to keep")
    c.add_argument("--out", help="write markdown here instead of stdout")

    s = sub.add_parser("steps", help="coarse per-step phase breakdown")
    s.add_argument("log")
    s.add_argument("--step")
    s.add_argument("--substring", action="store_true",
                   help="match step by substring instead of exact name")

    h = sub.add_parser("hostio", help="summarise vmstat / iostat sampler captures")
    h.add_argument("--host-vmstat")
    h.add_argument("--guest-vmstat")
    h.add_argument("--iostat")
    h.add_argument("--skip", type=int, default=0,
                   help="drop the first N samples (setup phase)")
    h.add_argument("--count", type=int, default=0,
                   help="analyse at most N samples after --skip (0 = all)")
    h.add_argument("--out")

    m = sub.add_parser("sampler", help="read-only host/guest operational sampler")
    m.add_argument("--seconds", type=float, default=900.0)
    m.add_argument("--interval", type=float, default=2.0)
    m.add_argument("--out", default="/tmp/runway-parity/sampler.jsonl")
    m.add_argument("--mariadb-socket")
    m.add_argument("--mariadb-defaults-file",
                   help="path to a 0600 client option file, if socket auth "
                        "is not enough; the path is safe in argv, its "
                        "contents are never exposed")
    f = sub.add_parser("fsync-probe", help="single-file fsync/commit microbenchmark")
    f.add_argument("--iters", type=int, default=200)
    f.add_argument("--bytes", type=int, default=4096)
    f.add_argument("--dir", help="scratch dir (default: fresh mkdtemp)")
    f.add_argument("--keep", action="store_true")

    r = sub.add_parser(
        "tree-read",
        help="cold vs warm read cost across a whole directory tree")
    r.add_argument("--tree", default="/opt/hostedtoolcache/Python",
                   help="directory to walk (default: the baked setup-python "
                        "tool cache, the same content class as site-packages)")
    r.add_argument("--suffix", action="append", dest="suffix",
                   default=(".py", ".pyc", ".so"),
                   help="filename suffix to include; repeatable, and "
                        "repeating it replaces the default set outright")
    r.add_argument("--threads", type=int, default=1,
                   help="concurrent readers; >1 tests whether the block path "
                        "is latency- or throughput-bound")
    r.add_argument("--host-cache-dropped", action="store_true",
                   help="record that the *other* kernel's page cache was "
                        "dropped between passes by hand; this tool cannot do it")

    k = sub.add_parser("cpu-probe",
                       help="single-core CPU speed + JIT availability probe")
    k.add_argument("--scale", type=float, default=0.25)
    k.add_argument("--repeats", type=int, default=3)

    args = ap.parse_args(argv)
    if args.cmd == "compare":
        base = profile_log(args.reference, args.step)
        cand = profile_log(args.candidate, args.step)
        text = compare(base, cand, args.top)
    elif args.cmd == "steps":
        step = args.step
        if step and getattr(args, "substring", False):
            step = None
        prof = profile_log(args.log, step)
        text = steps_report(prof)
    elif args.cmd == "hostio":
        text = hostio_report(args)
    elif args.cmd == "sampler":
        return sampler(args)
    elif args.cmd == "fsync-probe":
        return fsync_probe(args)
    elif args.cmd == "tree-read":
        return tree_read_probe(args)
    elif args.cmd == "cpu-probe":
        return cpu_probe(args)
    else:  # pragma: no cover
        ap.error("unknown command")

    if getattr(args, "out", None):
        with _open_private(args.out) as fh:
            fh.write(text + "\n")
        print(f"wrote {args.out} ({len(text)} bytes)")
    else:
        print(text)
    return 0


if __name__ == "__main__":
    sys.exit(main())