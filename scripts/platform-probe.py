#!/usr/bin/env python3
"""Per-operation latency probe for CI runner platforms.

Answers the questions that decide whether a self-hosted runner's test phase is
slower because of the machine or because of the interpreter build: interpreter
build flags, clock/timer cost, bytecode dispatch, allocation, and loopback
round-trip latency.

Strictly a reader. It opens one loopback listener on an ephemeral port, reads one
attribute under `/sys`, starts no container, pulls no image, and writes nothing.

What each field actually reports, because they are not all the same statistic:

- `clock_cost_*` is the *fastest* of three repetitions of `calls` reads.
- `sleep_overshoot_*` is the median and p95 of 40 sleeps in one pass.
- `interpreter_*` is the median of three repetitions.
- `rtt_loopback` is one pass of `rounds` round trips, so its median and p95 come
  from a single run and are bimodal on some hosts. Compare medians across whole
  runs, not within one.

The published-container-port path, which is the shape of a database reached
through a port Docker publishes, is deliberately not measured here: doing it
properly pulls an image into the very image store being measured, and a
throwaway echo container never accepted a connection on this guest, so the
figure was taken with a separate C harness instead. See the performance report.

Standard library only. Prints one JSON object per measurement.
"""

from __future__ import annotations

import argparse
import json
import os
import platform
import socket
import statistics
import sys
import sysconfig
import threading
import time

REPS = 3
# Ordered so the cheapest and most identifying runs first: a truncated capture is
# still worth something if it got this far.
SECTIONS = {
    "identity": "interpreter build, JIT state, platform and CPU count",
    "clocksource": "the kernel clocksource currently selected and available",
    "clock": "per-call cost of clock reads and the accuracy of sleeps",
    "interpreter": "bytecode dispatch and allocation throughput",
    "network": "loopback TCP round-trip latency",
}
ORDER = list(SECTIONS)


def emit(name, **kw):
    print(json.dumps(dict(measurement=name, **kw)), flush=True)


def median_of(fn, reps=REPS):
    samples = []
    for _ in range(reps):
        start = time.perf_counter()
        fn()
        samples.append(time.perf_counter() - start)
    return statistics.median(samples)


# --------------------------------------------------------------------------
# Platform and build identity
# --------------------------------------------------------------------------

def report_identity():
    keys = (
        "USE_COMPUTED_GOTOS", "HAVE_COMPUTED_GOTOS", "Py_GIL_DISABLED",
        "CONFIG_ARGS", "Py_DEBUG", "WITH_PYMALLOC",
    )
    config = {k: sysconfig.get_config_var(k) for k in keys}
    jit = None
    try:
        if hasattr(sys, "_jit"):
            jit = dict(available=sys._jit.is_available(),
                       enabled=sys._jit.is_enabled())
    except Exception as exc:                      # pragma: no cover
        jit = {"error": type(exc).__name__}
    emit("identity",
         python=sys.version.split()[0],
         python_full=sys.version.replace("\n", " "),
         executable=sys.executable,
         platform=platform.platform(),
         machine=platform.machine(),
         nproc=os.cpu_count(),
         computed_gotos=config["USE_COMPUTED_GOTOS"],
         config_args=str(config["CONFIG_ARGS"]).split(),
         jit=jit,
         python_jit_env=os.environ.get("PYTHON_JIT"))


def report_clocksource():
    base = "/sys/devices/system/clocksource/clocksource0"
    current = available = None
    try:
        current = open(f"{base}/current_clocksource").read().strip()
        available = open(f"{base}/available_clocksource").read().split()
    except OSError as exc:
        emit("clocksource", error=type(exc).__name__)
        return
    emit("clocksource", current=current, available=available)


# --------------------------------------------------------------------------
# Clock and timer cost
# --------------------------------------------------------------------------

def report_clock_cost():
    """Cost of one clock read, which is what a timestamped test pays per call.

    Frappe's result printer, its database writes and its retry loops all read
    the clock. Measured in Python because that is the layer the workload uses;
    CPython reaches clock_gettime through the vDSO, so a guest whose vDSO
    cannot serve the clock pays a full syscall per read here.
    """
    for name in ("monotonic", "perf_counter", "time"):
        fn = getattr(time, name)
        n = 200000
        best = None
        for _ in range(REPS):
            start = time.perf_counter()
            for _ in range(n):
                fn()
            elapsed = time.perf_counter() - start
            best = elapsed if best is None else min(best, elapsed)
        emit(f"clock_cost_{name}", ns_per_call=round(best / n * 1e9, 1),
             calls=n)

    for requested in (0.001, 0.010):
        overshoots = []
        for _ in range(40):
            start = time.perf_counter()
            time.sleep(requested)
            overshoots.append((time.perf_counter() - start - requested) * 1e6)
        overshoots.sort()
        emit(f"sleep_overshoot_{int(requested * 1000)}ms",
             median_us=round(statistics.median(overshoots), 1),
             p95_us=round(overshoots[int(0.95 * (len(overshoots) - 1))], 1),
             requested_us=round(requested * 1e6, 1))


# --------------------------------------------------------------------------
# Interpreter work
# --------------------------------------------------------------------------

def _dispatch(n):
    total = 0
    for i in range(n):
        total = total + (i ^ 0x5A) & 0xFF
    return total


def _allocate(n):
    sink = None
    for i in range(n):
        sink = {"k": i, "v": [i, i + 1, i + 2], "s": f"{i:04d}"}
    return sink


def report_interpreter():
    n = 2_000_000
    emit("interpreter_dispatch",
         ns_per_iter=round(median_of(lambda: _dispatch(n)) / n * 1e9, 2),
         iters=n)
    m = 400_000
    emit("interpreter_allocate",
         ns_per_iter=round(median_of(lambda: _allocate(m)) / m * 1e9, 2),
         iters=m)


# --------------------------------------------------------------------------
# Per-request network latency
# --------------------------------------------------------------------------

def _echo_server(sock):
    while True:
        try:
            conn, _ = sock.accept()
        except OSError:
            return
        with conn:
            try:
                while conn.recv(4096):
                    conn.sendall(b"x")
            except OSError:
                pass


def _rtt(host, port, rounds=2000):
    samples = []
    with socket.create_connection((host, port), timeout=5) as sock:
        sock.setsockopt(socket.IPPROTO_TCP, socket.TCP_NODELAY, 1)
        for _ in range(rounds):
            start = time.perf_counter()
            sock.sendall(b"ping")
            sock.recv(4096)
            samples.append((time.perf_counter() - start) * 1e6)
    samples.sort()
    return samples


def report_network():
    listener = socket.socket()
    listener.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    listener.bind(("127.0.0.1", 0))
    listener.listen(8)
    port = listener.getsockname()[1]
    threading.Thread(target=_echo_server, args=(listener,),
                     daemon=True).start()
    try:
        s = _rtt("127.0.0.1", port)
        emit("rtt_loopback",
             median_us=round(statistics.median(s), 2),
             p95_us=round(s[int(0.95 * (len(s) - 1))], 2),
             rounds=len(s))
    finally:
        listener.close()


def main(argv=None):
    parser = argparse.ArgumentParser(
        description="Measure per-operation latency on a CI runner platform.",
        epilog="With no arguments every section runs, which takes about a minute.")
    parser.add_argument("--section", action="append", choices=sorted(SECTIONS),
                        help="run only this section; repeatable (default: all)")
    parser.add_argument("--list-sections", action="store_true",
                        help="print the sections and their descriptions, then exit")
    args = parser.parse_args(argv)

    if args.list_sections:
        for name in ORDER:
            print(f"{name:14} {SECTIONS[name]}")
        return 0

    chosen = args.section or ORDER
    runners = {
        "identity": report_identity,
        "clocksource": report_clocksource,
        "clock": report_clock_cost,
        "interpreter": report_interpreter,
        "network": report_network,
    }
    # Each section is independent and optional. A probe that cannot take one
    # measurement has to say so and keep going: a diagnostic whose failure mode
    # is to abort loses every later measurement, and the caller cannot tell an
    # absent number from a crashed run.
    for name in chosen:
        try:
            runners[name]()
        except Exception as exc:
            emit("section_failed", section=name,
                 error=type(exc).__name__, detail=str(exc)[:200])
    print("DONE", flush=True)
    return 0


if __name__ == "__main__":
    sys.exit(main())
