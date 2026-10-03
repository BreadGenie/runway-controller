#!/usr/bin/env python3
"""Tests for scripts/profile-frappe-run.py.

Two areas are covered.

1. The log parser has to survive the two things that make CI logs awkward and
   that silently corrupt timings if handled wrongly:
     - ANSI colour codes, which GitHub log downloads store *literalised* as the
       two characters ``^[`` rather than as a real ESC byte.  A parser that
       only strips real ESC matches zero test results and reports every class
       as 0.00s.
     - Lines of runtime noise (warnings, tracebacks, replayed setUpClass
       output) printed *between* two results, which would otherwise be charged
       to whichever test happened to precede them.

2. The operational sampler.  It writes to a predictable path in a shared /tmp
   while normally running as root, shells out to a MariaDB client, and divides
   CPU seconds by a wall interval, so its path handling, its argv and its
   interval arithmetic are all pinned by tests here.

Every fixture is inline and deterministic; nothing reads a capture from a
private directory.  Run: python3 scripts/test_profile_frappe_run.py
"""

import errno
import importlib.util
import json
import os
import shutil
import subprocess
import sys
import tempfile
import types
import unittest
from datetime import datetime, timedelta, timezone
from unittest import mock

SCRIPT = os.path.join(os.path.dirname(os.path.abspath(__file__)),
                      "profile-frappe-run.py")

_spec = importlib.util.spec_from_file_location("profile_frappe_run", SCRIPT)
pfr = importlib.util.module_from_spec(_spec)
sys.modules["profile_frappe_run"] = pfr  # dataclasses needs the module registered
_spec.loader.exec_module(pfr)


def gh_line(job, step, ts, text):
    """Build one line in GitHub's tab-separated job log format.

    `ts` is an ISO timestamp with or without a trailing Z.
    """
    if not ts.endswith("Z"):
        ts += "Z"
    return f"{job}\t{step}\t{ts} {text}\n"


ESC = "\x1b[32m"           # real ESC form
LIT = "^[[32m"             # literalised form: ESC was replaced by "^[", so the
RESET_LIT = "^[[0m"        # sequence reads "^[" followed by "[32m"
RESET_ESC = "\x1b[0m"


class TimestampParsingTests(unittest.TestCase):
    def test_seven_fractional_digits_are_truncated(self):
        # GitHub emits 100ns precision; datetime only accepts microseconds.
        lines = list(pfr.parse_timestamped_lines(_write([
            gh_line("J", "S", "2026-10-03T07:00:00.1234567Z", "hello"),
        ])))
        self.assertEqual(len(lines), 1)
        self.assertEqual(lines[0].ts.microsecond, 123456)
        self.assertEqual(lines[0].text, "hello")

    def test_message_containing_tabs_is_preserved(self):
        lines = list(pfr.parse_timestamped_lines(_write([
            gh_line("J", "S", "2026-10-03T07:00:00.0000000Z", "a\tb\tc"),
        ])))
        self.assertEqual(lines[0].text, "a\tb\tc")

    def test_byte_order_mark_is_stripped(self):
        raw = "\ufeff" + gh_line("J", "S", "2026-10-03T07:00:00.0000000Z", "hi")
        lines = list(pfr.parse_timestamped_lines(_write([raw])))
        self.assertEqual(len(lines), 1)
        self.assertEqual(lines[0].text, "hi")

    def test_lines_without_a_timestamp_are_skipped(self):
        lines = list(pfr.parse_timestamped_lines(_write([
            "not a log line at all\n",
            gh_line("J", "S", "2026-10-03T07:00:00.0000000Z", "ok"),
        ])))
        self.assertEqual(len(lines), 1)


class AnsiStrippingTests(unittest.TestCase):
    def test_literal_caret_form_is_stripped(self):
        text = f"   {LIT} \u2714 {RESET_LIT} test_thing"
        self.assertEqual(pfr.ANSI_RE.sub("", text), "    \u2714  test_thing")

    def test_real_escape_form_is_stripped(self):
        text = f"   {ESC} \u2714 {RESET_ESC} test_thing"
        self.assertEqual(pfr.ANSI_RE.sub("", text), "    \u2714  test_thing")

    def test_result_survives_literal_ansi_end_to_end(self):
        log = [
            gh_line("J", "Run Tests", "2026-10-03T07:00:00.0000000Z",
                    "frappe.mod.TestThing"),
            gh_line("J", "Run Tests", "2026-10-03T07:00:01.5000000Z",
                    f"   {LIT} \u2714 {RESET_LIT} test_method"),
        ]
        prof = pfr.profile_log(_write(log), "Run Tests")
        self.assertEqual(prof.results_seen, 1)
        self.assertEqual(prof.classes["frappe.mod.TestThing"].tests[0].name,
                         "test_method")


class ResultRegexTests(unittest.TestCase):
    def test_pass_slow_with_reported_duration(self):
        m = pfr.RESULT_RE.match("   \u2714  test_thing (12.345s)")
        self.assertEqual(m.group(1), "test_thing")
        self.assertEqual(m.group(2), "12.345")

    def test_pass_fast_has_no_reported_duration(self):
        m = pfr.RESULT_RE.match("   \u2714  test_thing")
        self.assertEqual(m.group(1), "test_thing")
        self.assertIsNone(m.group(2))

    def test_failure_and_skip_markers(self):
        self.assertTrue(pfr.RESULT_RE.match("   \u2716  test_x"))
        self.assertTrue(pfr.RESULT_RE.match("   =  test_x"))

    def test_only_symbol_marks_and_equals_are_statuses(self):
        # frappe/testing/result.py styles its three statuses with Unicode
        # check/cross marks; "=" is the ASCII skip marker.  ASCII *letters* were
        # once accepted too, which made ordinary wrapped log lines parse as
        # results and inflated results_seen past the runner's own Tests: count.
        for mark in ("\u2714", "\u2718", "\u2716", "\u2717", "\u00d7", "\u2713",
                     "="):
            with self.subTest(mark=mark):
                self.assertIsNotNone(pfr.RESULT_RE.match(f"   {mark}  test_x"))
        for line in ("   x  test_x", "   u  test_u", "   a  test_a",
                     "   -  test_d", "   +  test_p", "   ?  test_e",
                     "   !  test_f"):
            with self.subTest(line=line):
                self.assertIsNone(pfr.RESULT_RE.match(line))

    def test_plain_log_lines_do_not_inflate_the_result_count(self):
        log = [
            gh_line("J", "Run Tests", "2026-10-03T07:00:00.0000000Z", "frappe.a.T"),
            gh_line("J", "Run Tests", "2026-10-03T07:00:01.0000000Z",
                    f"   {LIT} \u2714 {RESET_LIT} test_one"),
            gh_line("J", "Run Tests", "2026-10-03T07:00:01.5000000Z",
                    "x 3 checks skipped by the fixture"),
            gh_line("J", "Run Tests", "2026-10-03T07:00:02.0000000Z",
                    "   =  test_skipped"),
        ]
        prof = pfr.profile_log(_write(log), "Run Tests")
        self.assertEqual(prof.results_seen, 2)
        self.assertEqual([c.name for c in prof.classes["frappe.a.T"].tests],
                         ["test_one", "test_skipped"])

    def test_warning_line_is_not_a_result(self):
        # A DeprecationWarning path must never parse as a test result.
        self.assertIsNone(pfr.RESULT_RE.match(
            "env/lib/python3.14/site-packages/premailer/premailer.py:599: "
            "DeprecationWarning: codecs.open() is deprecated"))


class ProfileTests(unittest.TestCase):
    def _three_tests(self):
        return [
            gh_line("J", "Run Tests", "2026-10-03T07:00:00.0000000Z",
                    "frappe.mod.TestA"),
            gh_line("J", "Run Tests", "2026-10-03T07:00:01.0000000Z",
                    f"   {LIT} \u2714 {RESET_LIT} test_one"),
            gh_line("J", "Run Tests", "2026-10-03T07:00:04.0000000Z",
                    f"   {LIT} \u2714 {RESET_LIT} test_two"),
        ]

    def test_noise_between_results_is_charged_to_the_running_test(self):
        log = self._three_tests()
        log.insert(2, gh_line(
            "J", "Run Tests", "2026-10-03T07:00:02.0000000Z",
            "site-packages/x.py:12: DeprecationWarning: blah"))
        log.insert(3, gh_line(
            "J", "Run Tests", "2026-10-03T07:00:02.1000000Z",
            "  \u25b8  some captured setUpClass output"))
        prof = pfr.profile_log(_write(log), "Run Tests")
        cases = prof.classes["frappe.mod.TestA"].tests
        self.assertEqual(cases[0].dur_max, 1.0)
        # test_two absorbs the 1s of warning output, so dur_max is the full 3s
        # while dur_min only sees the 1.9s since the noise line.
        self.assertAlmostEqual(cases[1].dur_max, 3.0)
        self.assertAlmostEqual(cases[1].dur_min, 1.9)
        # The reported duration wins when the runner printed one.
        self.assertEqual(cases[1].best, cases[1].dur_max)

    def test_reported_duration_overrides_the_gap_estimate(self):
        log = self._three_tests()
        log[2] = gh_line("J", "Run Tests", "2026-10-03T07:00:09.0000000Z",
                         f"   {LIT} \u2714 {RESET_LIT} test_two (8.750s)")
        prof = pfr.profile_log(_write(log), "Run Tests")
        case = prof.classes["frappe.mod.TestA"].tests[1]
        self.assertEqual(case.reported_s, 8.75)
        self.assertEqual(case.best, 8.75)

    def test_method_names_are_scoped_to_the_current_class(self):
        # The same method name in two classes must not collide.
        log = [
            gh_line("J", "Run Tests", "2026-10-03T07:00:00.0000000Z", "frappe.a.T1"),
            gh_line("J", "Run Tests", "2026-10-03T07:00:01.0000000Z",
                    f"   {LIT} \u2714 {RESET_LIT} test_dup"),
            gh_line("J", "Run Tests", "2026-10-03T07:00:02.0000000Z", "frappe.b.T2"),
            gh_line("J", "Run Tests", "2026-10-03T07:00:03.0000000Z",
                    f"   {LIT} \u2714 {RESET_LIT} test_dup"),
        ]
        prof = pfr.profile_log(_write(log), "Run Tests")
        self.assertEqual(prof.classes["frappe.a.T1"].tests[0].name, "test_dup")
        self.assertEqual(prof.classes["frappe.b.T2"].tests[0].name, "test_dup")

    def test_step_filter_is_exact_not_substring(self):
        # "Post Environment Setup" must not be swept in by "Environment Setup".
        log = [
            gh_line("J", "Environment Setup", "2026-10-03T07:00:00.0000000Z", "a"),
            gh_line("J", "Post Environment Setup",
                    "2026-10-03T07:10:00.0000000Z", "b"),
        ]
        prof = pfr.profile_log(_write(log), "Environment Setup")
        self.assertEqual(len(prof.steps), 1)

    def test_summary_line_records_reported_test_count(self):
        log = [
            gh_line("J", "Run Tests", "2026-10-03T07:00:00.0000000Z",
                    "Tests: 708, Failing: 0, Errors: 0"),
        ]
        prof = pfr.profile_log(_write(log), "Run Tests")
        self.assertEqual(prof.tests_reported, 708)

    def test_empty_log_does_not_raise(self):
        prof = pfr.profile_log(_write([]), "Run Tests")
        self.assertEqual(prof.results_seen, 0)
        self.assertEqual(prof.test_wall, 0.0)


class CompareTests(unittest.TestCase):
    def test_matches_by_class_and_method_and_ranks_by_gap(self):
        base = [
            gh_line("J", "Run Tests", "2026-10-03T07:00:00.0000000Z", "frappe.a.T"),
            gh_line("J", "Run Tests", "2026-10-03T07:00:01.0000000Z",
                    f"   {LIT} \u2714 {RESET_LIT} test_small"),
            gh_line("J", "Run Tests", "2026-10-03T07:00:02.0000000Z",
                    f"   {LIT} \u2714 {RESET_LIT} test_big"),
        ]
        cand = [
            gh_line("J", "Run Tests", "2026-10-03T07:00:00.0000000Z", "frappe.a.T"),
            gh_line("J", "Run Tests", "2026-10-03T07:00:02.0000000Z",
                    f"   {LIT} \u2714 {RESET_LIT} test_small"),
            gh_line("J", "Run Tests", "2026-10-03T07:00:12.0000000Z",
                    f"   {LIT} \u2714 {RESET_LIT} test_big"),
        ]
        report = pfr.compare(pfr.profile_log(_write(base), "Run Tests"),
                             pfr.profile_log(_write(cand), "Run Tests"), 10)
        self.assertIn("frappe.a.T", report)
        # test_big gained 9s, test_small gained 1s; the ranking must lead with
        # the bigger gap.
        per_test = report.split("## Per-test gap")[1].split("##")[0]
        rows = [ln for ln in per_test.splitlines() if "test_" in ln]
        self.assertIn("test_big", rows[0])
        self.assertIn("uniform", per_test.lower() + report.lower())


class SamplerParsingTests(unittest.TestCase):
    """Inline captures.  Two sysstat column layouts, as in a real capture:
    a short one and a longer one that adds rrqm/s..%rrqm and d/s..%drqm.
    """
    SHORT_HEADER = ("Device            r/s     w/s     wkB/s   r_await   w_await  "
                    "aqu-sz  %util\n")
    SHORT_ROWS = (
        "vda              0.20    1.38    30.71     0.14     0.31    0.09   0.01\n"
        "vda            293.50   35.00  2778.00     0.18     0.14    0.06   2.25\n"
    )
    LONG_HEADER = ("Device            r/s     rkB/s   rrqm/s  %rrqm r_await rareq-sz  "
                   "    w/s     wkB/s   wrqm/s  %wrqm w_await wareq-sz     d/s"
                   "     dkB/s   drqm/s  %drqm d_await dareq-sz     f/s f_await"
                   "  aqu-sz  %util\n")
    LONG_ROWS = (
        "vda              0.20     10.02     0.02   8.53    0.14    51.08    1.38"
        "     30.71     0.44  23.99    0.31    22.21    0.03    327.83     0.00"
        "   0.00    0.04 11376.30    0.16    0.09    0.00   0.01\n"
    )
    # 17 numeric columns, as `vmstat -w` emits them (indented run header off).
    VMSTAT_ROWS = (
        "   2    0            0      5455428        13076       454172    0    0"
        " 13669  4144 1650    7  13  11  67   4   5   0\n"
        "   1    0            0      5427872        16352       474412    0    0"
        "  1804  8266 1496 2334   17   7  74   2   2   0\n"
    )

    def test_iostat_uses_its_own_header_as_schema(self):
        text = self.LONG_HEADER + self.LONG_ROWS
        cols = self.LONG_HEADER.split()[1:]
        row = self.LONG_ROWS.splitlines()[0].split()
        devices = pfr.parse_iostat_log(_write(text.splitlines(keepends=True)))
        self.assertEqual(list(devices["vda"][0]), cols)
        self.assertEqual(devices["vda"][0]["w_await"],
                         row[cols.index("w_await") + 1])

    def test_iostat_tolerates_a_different_column_layout(self):
        # A later sysstat adds rrqm/s..%rrqm; the schema must come from the
        # header rather than from hardcoded offsets.
        text = self.SHORT_HEADER + self.SHORT_ROWS
        devices = pfr.parse_iostat_log(_write(text.splitlines(keepends=True)))
        self.assertEqual(len(devices["vda"]), 2)
        self.assertNotIn("rrqm/s", devices["vda"][0])
        self.assertEqual(float(devices["vda"][1]["%util"]), 2.25)

    def test_iostat_tolerates_repeated_header_blocks(self):
        text = self.SHORT_HEADER + self.SHORT_ROWS + self.SHORT_HEADER + self.SHORT_ROWS
        devices = pfr.parse_iostat_log(_write(text.splitlines(keepends=True)))
        self.assertEqual(len(devices["vda"]), 4)
        # The two blocks are parsed under their own header, so each repeats.
        self.assertEqual(devices["vda"][0], devices["vda"][2])
        self.assertEqual(devices["vda"][1], devices["vda"][3])

    def test_iostat_rejects_rows_that_do_not_fit_the_header(self):
        text = (self.SHORT_HEADER + "vda 0.20 1.38\n"
                + self.SHORT_ROWS)
        devices = pfr.parse_iostat_log(_write(text.splitlines(keepends=True)))
        self.assertEqual(len(devices["vda"]), 2)

    def test_vmstat_rows_need_seventeen_numeric_columns(self):
        rows = self.VMSTAT_ROWS.splitlines(keepends=True)
        mixed = [rows[0], "bad line\n", rows[1]]
        parsed = pfr.parse_vmstat_log(_write(mixed))
        self.assertEqual(len(parsed), 2)
        self.assertEqual(list(parsed[0]), pfr.VMSTAT_COLS)
        self.assertEqual(parsed[0]["bo"], float(rows[0].split()[9]))
        self.assertEqual(parsed[0]["st"], float(rows[0].split()[16]))
        # "free" must survive as a number, not as the header word.
        self.assertEqual(parsed[1]["free"], 5427872.0)

    def test_vmstat_rejects_non_numeric_first_column(self):
        text = "procs -----------memory----------\n" + self.VMSTAT_ROWS
        self.assertEqual(len(pfr.parse_vmstat_log(
            _write(text.splitlines(keepends=True)))), 2)


def _sampler_args(out, seconds=0.1, interval=0.05, socket=None, defaults=None):
    return types.SimpleNamespace(out=out, seconds=seconds, interval=interval,
                                 mariadb_socket=socket,
                                 mariadb_defaults_file=defaults)


def _records(path):
    with open(path, encoding="utf-8") as handle:
        return [json.loads(ln) for ln in handle if ln.strip()]


class SamplerOutputSafetyTests(unittest.TestCase):
    """The sampler creates a new capture, normally as root on the metal node.

    Two properties are pinned here.  The containing directory has to be private
    to us -- a capture in a shared directory is readable by every other account,
    which defeats the point of writing one.  And only ever a *new* file: nothing
    already sitting at the output path is opened, truncated or reused, whatever
    it is.  Owning a 0600 single-link file does not prove it was a previous
    capture rather than some other private file we have no business replacing,
    so there is no reuse case at all.
    """

    def test_output_path_that_is_a_symlink_is_refused(self):
        with tempfile.TemporaryDirectory() as scratch:
            target = os.path.join(scratch, "important.conf")
            with open(target, "w", encoding="utf-8") as fh:
                fh.write("do not touch me")
            os.chmod(target, 0o600)
            link = os.path.join(scratch, "sampler.jsonl")
            os.symlink(target, link)

            with self.assertRaises(FileExistsError):
                pfr.sampler(_sampler_args(link))

            with open(target, encoding="utf-8") as fh:
                self.assertEqual(fh.read(), "do not touch me")
            # The planted symlink itself is left in place, not replaced.
            self.assertTrue(os.path.islink(link))
            self.assertEqual(os.stat(target).st_size, len("do not touch me"))

    def test_hardlinked_output_path_is_refused_and_both_names_survive(self):
        # Truncating a hardlink writes through to every other name for that
        # inode, so a planted link must leave both names untouched.
        with tempfile.TemporaryDirectory() as scratch:
            victim = os.path.join(scratch, "victim.conf")
            with open(victim, "w", encoding="utf-8") as fh:
                fh.write("PRECIOUS")
            os.chmod(victim, 0o600)
            link = os.path.join(scratch, "sampler.jsonl")
            os.link(victim, link)

            with self.assertRaises(FileExistsError):
                pfr.sampler(_sampler_args(link))

            for name in (victim, link):
                with open(name, encoding="utf-8") as fh:
                    self.assertEqual(fh.read(), "PRECIOUS")
            self.assertEqual(os.stat(victim).st_nlink, 2)

    def test_an_existing_private_file_we_own_is_still_refused(self):
        # 0600, single link, ours, in our own private directory -- and still
        # refused, because none of that shows it was a capture of ours.
        with tempfile.TemporaryDirectory() as scratch:
            out = os.path.join(scratch, "sampler.jsonl")
            with open(out, "w", encoding="utf-8") as fh:
                fh.write("A PREVIOUS CAPTURE\n")
            os.chmod(out, 0o600)
            before = os.stat(out)

            with self.assertRaises(FileExistsError) as ctx:
                pfr.sampler(_sampler_args(out))
            self.assertIn("only creates new captures", str(ctx.exception))

            with open(out, encoding="utf-8") as fh:
                self.assertEqual(fh.read(), "A PREVIOUS CAPTURE\n")
            self.assertEqual(os.stat(out).st_size, before.st_size)
            self.assertEqual(os.stat(out).st_mode & 0o777, 0o600)

    def test_a_second_run_into_the_same_path_is_refused(self):
        # Re-running is the case the new-captures-only rule bites: the first
        # capture stays exactly as written and the second does not land.
        with tempfile.TemporaryDirectory() as scratch:
            out = os.path.join(scratch, "sampler.jsonl")
            self.assertEqual(pfr.sampler(_sampler_args(out)), 0)
            with open(out, encoding="utf-8") as fh:
                first = fh.read()
            self.assertGreaterEqual(len(first.splitlines()), 1)

            with self.assertRaises(FileExistsError):
                pfr.sampler(_sampler_args(out))
            with open(out, encoding="utf-8") as fh:
                self.assertEqual(fh.read(), first)

    def test_output_path_that_is_not_a_regular_file_is_refused(self):
        # Notably a fifo: nothing is ever opened here, so there is no way to
        # block on one waiting for a reader to turn up.
        with tempfile.TemporaryDirectory() as scratch:
            for kind, build in (("fifo", os.mkfifo), ("directory", os.mkdir)):
                out = os.path.join(scratch, f"capture-{kind}")
                build(out, 0o700)
                before = sorted(os.listdir(scratch))
                with self.subTest(kind=kind):
                    with self.assertRaises(FileExistsError):
                        pfr.sampler(_sampler_args(out))
                # Refused before anything was created or written into.
                self.assertEqual(sorted(os.listdir(scratch)), before)
                if kind == "directory":
                    self.assertEqual(os.listdir(out), [])

    def test_shared_output_directory_is_refused(self):
        with tempfile.TemporaryDirectory() as scratch:
            shared = os.path.join(scratch, "shared")
            os.mkdir(shared, 0o777)
            os.chmod(shared, 0o775)
            with self.assertRaises(PermissionError):
                pfr.sampler(_sampler_args(os.path.join(shared, "sampler.jsonl")))
            self.assertEqual(os.listdir(shared), [])

    def test_shared_tmp_subdirectory_is_refused_like_the_default_path(self):
        # /tmp itself is legitimately 1777 and its sticky bit protects what we
        # hold.  What is not acceptable is the shared per-run directory under it
        # that the default --out points at, so that shape is refused too.
        base = os.path.join(tempfile.gettempdir(), f"pfr-shared-{os.getpid()}")
        self.addCleanup(shutil.rmtree, base, ignore_errors=True)
        os.mkdir(base, 0o777)
        os.chmod(base, 0o777)
        with self.assertRaises(PermissionError):
            pfr.sampler(_sampler_args(os.path.join(base, "sampler.jsonl")))
        self.assertEqual(os.listdir(base), [])

    def test_output_directory_owned_by_another_user_is_refused(self):
        # 0700 and private, but not ours: someone else owns it and can replace
        # it, so it is not a place to put a capture.  geteuid is patched rather
        # than chown'd so the case is exercised whoever runs the suite.
        with tempfile.TemporaryDirectory() as scratch:
            theirs = os.path.join(scratch, "theirs")
            os.mkdir(theirs, 0o700)
            os.chmod(theirs, 0o700)
            other = os.stat(theirs).st_uid + 1
            with mock.patch.object(pfr.os, "geteuid", return_value=other):
                with self.assertRaises(PermissionError) as ctx:
                    pfr.sampler(_sampler_args(os.path.join(theirs, "s.jsonl")))
            self.assertIn("owned by uid", str(ctx.exception))
            self.assertEqual(os.listdir(theirs), [])

    def test_symlinked_output_directory_is_refused(self):
        with tempfile.TemporaryDirectory() as scratch:
            real = os.path.join(scratch, "real")
            os.mkdir(real, 0o700)
            os.chmod(real, 0o700)
            link = os.path.join(scratch, "link")
            os.symlink(real, link)

            with self.assertRaises(OSError) as ctx:
                pfr.sampler(_sampler_args(os.path.join(link, "sampler.jsonl")))
            self.assertEqual(ctx.exception.errno, errno.ELOOP)
            # Refused before the directory was written into.
            self.assertEqual(os.listdir(real), [])

    def test_the_capture_is_opened_with_excl_and_never_truncated(self):
        # These three flags are the whole of the guarantee: O_EXCL refuses an
        # existing entry, O_NOFOLLOW stops a symlink being written through, and
        # the absence of O_TRUNC means nothing already there can be shortened.
        seen = []
        real_open = os.open
        marker = [""]

        def spy(path, flags, *a, **kw):
            if os.path.abspath(str(path)) == os.path.abspath(marker[0]):
                seen.append(flags)
            return real_open(path, flags, *a, **kw)

        with tempfile.TemporaryDirectory() as scratch:
            out = os.path.join(scratch, "sampler.jsonl")
            marker[0] = out
            with mock.patch.object(pfr.os, "open", spy):
                self.assertEqual(pfr.sampler(_sampler_args(out)), 0)

        self.assertEqual(len(seen), 1, "the capture must be opened exactly once")
        flags = seen[0]
        self.assertTrue(flags & os.O_CREAT)
        self.assertTrue(flags & os.O_EXCL)
        self.assertTrue(flags & os.O_NOFOLLOW)
        self.assertFalse(flags & os.O_TRUNC)

    def test_a_umask_that_strips_owner_bits_cannot_narrow_the_output(self):
        # os.open's mode is masked by the umask, so an explicit fchmod is the
        # only thing holding 0600 against a hostile umask.  The parent already
        # exists here so the umask cannot stop us writing into it at all.
        with tempfile.TemporaryDirectory() as scratch:
            out = os.path.join(scratch, "sampler.jsonl")
            old = os.umask(0o200)  # would drop owner-write from the new file
            try:
                self.assertEqual(pfr.sampler(_sampler_args(out)), 0)
            finally:
                os.umask(old)
            self.assertEqual(os.stat(out).st_mode & 0o777, 0o600)

    def test_a_umask_cannot_narrow_a_created_directory(self):
        with tempfile.TemporaryDirectory() as scratch:
            nested = os.path.join(scratch, "made-here")
            old = os.umask(0o200)
            try:
                self.assertEqual(
                    pfr.sampler(_sampler_args(os.path.join(nested, "s.jsonl"))), 0)
            finally:
                os.umask(old)
            self.assertEqual(os.stat(nested).st_mode & 0o777, 0o700)

    def test_output_is_created_private_to_the_owner(self):
        with tempfile.TemporaryDirectory() as scratch:
            # A nested path, so the parent directory has to be created too.
            out = os.path.join(scratch, "nested", "sampler.jsonl")
            self.assertEqual(pfr.sampler(_sampler_args(out)), 0)
            self.assertEqual(os.stat(out).st_mode & 0o777, 0o600)
            # Whatever umask is in force, the directory must not be reachable
            # by anyone else.
            self.assertEqual(os.stat(os.path.dirname(out)).st_mode & 0o077, 0)
            self.assertGreaterEqual(len(_records(out)), 1)

    def test_a_group_readable_umask_cannot_widen_the_output(self):
        with tempfile.TemporaryDirectory() as scratch:
            out = os.path.join(scratch, "sampler.jsonl")
            old = os.umask(0o022)
            try:
                self.assertEqual(pfr.sampler(_sampler_args(out)), 0)
            finally:
                os.umask(old)
            self.assertEqual(os.stat(out).st_mode & 0o777, 0o600)

    def test_a_permissive_umask_cannot_widen_a_created_directory(self):
        with tempfile.TemporaryDirectory() as scratch:
            nested = os.path.join(scratch, "a", "b")
            old = os.umask(0o000)
            try:
                self.assertEqual(
                    pfr.sampler(_sampler_args(os.path.join(nested, "s.jsonl"))), 0)
            finally:
                os.umask(old)
            self.assertEqual(os.stat(nested).st_mode & 0o777, 0o700)


class SamplerTimingTests(unittest.TestCase):
    """pct_one_core divides CPU seconds by the wall gap between the two /proc
    snapshots.  That gap is *not* the sleep: it also contains the previous
    sample's own reads and its MariaDB query, which alone can take seconds."""

    PROC_READ_S = 0.25
    QUERY_S = 3.0
    CPU_S = 0.5

    def _clock(self):
        state = [0.0]

        def monotonic():
            return state[0]

        def sleep(seconds):
            state[0] += seconds

        return state, monotonic, sleep

    def test_interval_covers_proc_reads_and_the_mariadb_query(self):
        state, monotonic, sleep = self._clock()
        hz = os.sysconf("SC_CLK_TCK")
        ticks = max(1, round(self.CPU_S * hz))
        snap = [{"pid": 1, "comm": "worker", "state": "R", "utime": 0,
                 "stime": 0}]
        busy = [{"pid": 1, "comm": "worker", "state": "R", "utime": ticks,
                 "stime": 0}]
        snapshots = iter([snap, busy, busy])
        calls = [0]

        def fake_read():
            calls[0] += 1
            state[0] += self.PROC_READ_S
            return next(snapshots)

        def fake_mariadb(_socket, _defaults=None):
            state[0] += self.QUERY_S
            return {"Innodb_data_fsyncs": 7}

        with tempfile.TemporaryDirectory() as scratch:
            out = os.path.join(scratch, "sampler.jsonl")
            args = _sampler_args(out, seconds=1.0, interval=0.5)
            with mock.patch.object(pfr.time, "monotonic", monotonic), \
                 mock.patch.object(pfr.time, "sleep", sleep), \
                 mock.patch.object(pfr, "_read_ps_once", fake_read), \
                 mock.patch.object(pfr, "_mariadb_status", fake_mariadb):
                self.assertEqual(pfr.sampler(args), 0)
            records = _records(out)

        self.assertEqual(len(records), 2)

        # Sample 1: 0.5s of sleep plus the 0.25s the opening /proc read took.
        # Counting sleeps alone would have reported 0.5.
        self.assertAlmostEqual(records[0]["interval_s"], 0.75, places=3)
        # Sample 2: the interval must also swallow the 3s MariaDB query that
        # ran after sample 1.  Sleep alone would have reported 0.2.
        self.assertAlmostEqual(records[1]["interval_s"], 3.45, places=3)
        self.assertGreater(records[1]["interval_s"], self.QUERY_S)

        # And so pct_one_core must be divided by that same real gap: 0.5s of
        # CPU over 0.75s is 66.7% of one core, not the 100% that dividing by
        # the requested sleep would have claimed.
        row = records[0]["procs"][0]
        self.assertAlmostEqual(row["cpu_s"], self.CPU_S, places=3)
        self.assertAlmostEqual(
            row["pct_one_core"],
            round(100 * row["cpu_s"] / records[0]["interval_s"], 1), places=1)
        self.assertLess(row["pct_one_core"], 100.0)

    def test_timestamps_are_utc_under_a_non_utc_timezone(self):
        # The only wall-clock field in a record, so it has to be unambiguous
        # without knowing the operator's TZ.
        with tempfile.TemporaryDirectory() as scratch:
            out = os.path.join(scratch, "sampler.jsonl")
            done = subprocess.run(
                [sys.executable, SCRIPT, "sampler", "--seconds", "0.1",
                 "--interval", "0.05", "--out", out],
                capture_output=True, text=True, timeout=120,
                env=dict(os.environ, TZ="America/New_York"))
            self.assertEqual(done.returncode, 0, done.stderr)
            records = _records(out)

        self.assertGreaterEqual(len(records), 1)
        for record in records:
            self.assertTrue(record["t"].endswith("+00:00"), record["t"])
            stamp = datetime.fromisoformat(record["t"])
            self.assertEqual(stamp.utcoffset(), timedelta(0))
            # Under TZ=America/New_York a local timestamp would be four hours
            # out; anything within two minutes is genuinely UTC now.
            self.assertLess(
                abs((datetime.now(timezone.utc) - stamp).total_seconds()), 120)

    def test_records_are_json_lines_with_the_expected_shape(self):
        with tempfile.TemporaryDirectory() as scratch:
            out = os.path.join(scratch, "sampler.jsonl")
            self.assertEqual(pfr.sampler(_sampler_args(out)), 0)
            with open(out, encoding="utf-8") as handle:
                lines = handle.read().splitlines()
        self.assertGreaterEqual(len(lines), 1)
        record = json.loads(lines[0])
        for key in ("t", "monotonic", "interval_s", "loadavg", "meminfo",
                    "vmstat", "procs", "mariadb"):
            self.assertIn(key, record)
        self.assertGreater(record["interval_s"], 0)


class MariaDBCredentialTests(unittest.TestCase):
    """Anything on a command line is world-readable in /proc/<pid>/cmdline for
    as long as the process lives, and the sampler runs once per interval for
    hours as root.  No credential may reach argv."""

    SOCKET = "/run/mysqld/mysqld.sock"

    def _call(self, socket=SOCKET, defaults=None):
        seen = {}

        class Done:
            returncode = 0
            stdout = ""
            stderr = ""

        def fake_run(cmd, **kwargs):
            seen["cmd"] = list(cmd)
            return Done()

        with mock.patch("shutil.which", return_value="/usr/bin/mariadb"), \
             mock.patch("subprocess.run", side_effect=fake_run):
            result = pfr._mariadb_status(socket, defaults)
        return seen.get("cmd"), result

    def test_no_password_is_ever_placed_in_argv(self):
        cmd, result = self._call()
        self.assertIsNotNone(cmd, "the client was never invoked")
        self.assertNotIn("error", result)
        leaked = [a for a in cmd
                  if "pass" in a.lower() or a.startswith("-p") or a == "-p"]
        self.assertEqual(leaked, [], f"credential-shaped argv entry: {cmd}")
        self.assertIn("--protocol=socket", cmd)

    def test_socket_auth_needs_no_option_file(self):
        cmd, _ = self._call()
        self.assertFalse([a for a in cmd if "defaults-file" in a])
        self.assertIn(f"--socket={self.SOCKET}", cmd)

    def test_ambient_default_files_are_not_consulted(self):
        # Exactly one of these two, always first, never both: the client reads
        # the first option and nothing ambient leaks in.
        cmd, _ = self._call()
        self.assertEqual(cmd[1], "--no-defaults")
        self.assertEqual(len([a for a in cmd if "defaults" in a]), 1)

    def test_option_file_is_referenced_by_path_only(self):
        # --defaults-file, not the program-level --defaults-extra-file spelling:
        # this client rejects the latter as an unknown variable.  Either way it
        # has to come first, since that is the file the client reads.
        with tempfile.TemporaryDirectory() as scratch:
            path = os.path.join(scratch, "my.cnf")
            with open(path, "w", encoding="utf-8") as fh:
                fh.write('[client]\npassword="S3cretPa55"\n')
            os.chmod(path, 0o600)
            cmd, result = self._call(defaults=path)
        self.assertNotIn("error", result)
        self.assertEqual(cmd[1], f"--defaults-file={path}")
        self.assertNotIn("S3cretPa55", " ".join(cmd))
        self.assertFalse([a for a in cmd if "defaults-extra-file" in a])

    def test_group_readable_option_file_is_refused_before_running_anything(self):
        with tempfile.TemporaryDirectory() as scratch:
            path = os.path.join(scratch, "my.cnf")
            with open(path, "w", encoding="utf-8") as fh:
                fh.write('[client]\npassword="S3cretPa55"\n')
            os.chmod(path, 0o644)
            cmd, result = self._call(defaults=path)
        self.assertIsNone(cmd, "the client ran despite an exposed option file")
        self.assertIn("error", result)
        self.assertIn("0644", result["error"])

    def test_missing_option_file_is_reported_not_raised(self):
        cmd, result = self._call(defaults="/nonexistent/my.cnf")
        self.assertIsNone(cmd)
        self.assertIn("error", result)

    def test_the_password_option_is_gone_from_the_cli(self):
        with mock.patch("sys.stderr"):
            with self.assertRaises(SystemExit):
                pfr.main(["sampler", "--mariadb-password", "S3cretPa55"])


class ProbeSafetyTests(unittest.TestCase):
    def test_fsync_probe_only_touches_its_own_directory(self):
        with tempfile.TemporaryDirectory() as scratch:
            ns = pfr.main(["fsync-probe", "--iters", "5", "--dir", scratch])
            self.assertEqual(ns, 0)
            self.assertEqual(os.listdir(scratch), ["probe.bin"])

    def test_fsync_probe_cleans_up_its_temp_dir(self):
        out = subprocess.run(
            [sys.executable, SCRIPT, "fsync-probe", "--iters", "3"],
            capture_output=True, text=True, check=True)
        report = json.loads(out.stdout)
        self.assertFalse(os.path.exists(report["dir"]))

    def test_a_second_probe_into_the_same_dir_is_refused(self):
        # probe.bin is created, never reused, so re-using --dir would overwrite
        # the previous run's numbers; it is refused instead.
        with tempfile.TemporaryDirectory() as scratch:
            self.assertEqual(
                pfr.main(["fsync-probe", "--iters", "2", "--dir", scratch]), 0)
            before = os.stat(os.path.join(scratch, "probe.bin")).st_size
            with self.assertRaises(FileExistsError):
                pfr.main(["fsync-probe", "--iters", "2", "--dir", scratch])
            self.assertEqual(os.stat(os.path.join(scratch, "probe.bin")).st_size,
                             before)

    def test_probe_file_is_private(self):
        with tempfile.TemporaryDirectory() as scratch:
            self.assertEqual(
                pfr.main(["fsync-probe", "--iters", "2", "--dir", scratch]), 0)
            probe = os.path.join(scratch, "probe.bin")
            self.assertEqual(os.stat(probe).st_mode & 0o777, 0o600)

    def test_fs_targets_a_private_directory(self):
        # probe.bin is a raw capture too, so --dir is held to the same rule.
        with tempfile.TemporaryDirectory() as scratch:
            shared = os.path.join(scratch, "shared")
            os.mkdir(shared, 0o777)
            os.chmod(shared, 0o777)
            with self.assertRaises(PermissionError):
                pfr.main(["fsync-probe", "--iters", "2", "--dir", shared])
            self.assertEqual(os.listdir(shared), [])

    def test_probe_scratch_dir_is_created_private(self):
        base = os.path.join(tempfile.gettempdir(), f"pfr-probe-{os.getpid()}")
        shutil.rmtree(base, ignore_errors=True)
        target = os.path.join(base, "deep", "scratch")
        try:
            self.assertEqual(
                pfr.main(["fsync-probe", "--iters", "2", "--dir", target]), 0)
            self.assertEqual(os.stat(target).st_mode & 0o777, 0o700)
        finally:
            shutil.rmtree(base, ignore_errors=True)

    def test_probe_refuses_a_symlinked_scratch_file(self):
        with tempfile.TemporaryDirectory() as scratch:
            target = os.path.join(scratch, "victim")
            with open(target, "w", encoding="utf-8") as fh:
                fh.write("PRECIOUS")
            os.chmod(target, 0o600)
            os.symlink(target, os.path.join(scratch, "probe.bin"))
            with self.assertRaises(OSError):
                pfr.main(["fsync-probe", "--iters", "2", "--dir", scratch])
            with open(target, encoding="utf-8") as fh:
                self.assertEqual(fh.read(), "PRECIOUS")

    def test_cpu_probe_runs_and_reports_jit_status(self):
        out = subprocess.run(
            [sys.executable, SCRIPT, "cpu-probe", "--scale", "0.01",
             "--repeats", "1"],
            capture_output=True, text=True, check=True)
        report = json.loads(out.stdout)
        self.assertIn("jit", report)
        self.assertGreater(report["integer_ops_per_s"], 0)
        self.assertGreaterEqual(report["online_cpus"], 1)


class RealLogRegressionTests(unittest.TestCase):
    """Guards the 708-test invariant against real paired job logs.

    Opt-in and off by default: these logs are a local artifact of one
    investigation, not a fixture this suite owns.  Point PFR_BASELINE_DIR at
    the directory holding baseline-github.log and baseline-runway.log to run
    them; otherwise they are skipped, so the suite stays deterministic on a
    machine that has never seen those captures.
    """

    def setUp(self):
        base = os.environ.get("PFR_BASELINE_DIR", "")
        if not base:
            self.skipTest("set PFR_BASELINE_DIR to the paired baseline logs")
        self.gh = os.path.join(base, "baseline-github.log")
        self.rw = os.path.join(base, "baseline-runway.log")
        for path in (self.gh, self.rw):
            if not os.path.exists(path):
                self.skipTest(f"{path} not present")

    def test_708_records_in_both_logs(self):
        for path in (self.gh, self.rw):
            prof = pfr.profile_log(path, "Run Tests")
            self.assertEqual(prof.results_seen, 708, path)
            self.assertEqual(prof.tests_reported, 708, path)
            self.assertEqual(len(prof.classes), 123, path)
            self.assertEqual(len(prof.orphan_tests), 0, path)

    def test_test_phase_is_slower_on_runway(self):
        gh = pfr.profile_log(self.gh, "Run Tests")
        rw = pfr.profile_log(self.rw, "Run Tests")
        self.assertLess(gh.test_wall, rw.test_wall)
        # The documented ratio for this pair is about 1.25x.
        self.assertLess(rw.test_wall / gh.test_wall, 1.4)


# Fixture logs are written to disk because the parser reads paths.  They are
# tracked here and removed in tearDownModule: a run makes ~30 of them and they
# would otherwise pile up in /tmp for good.
_TEMP_LOGS: list[str] = []


def _write(lines):
    handle = tempfile.NamedTemporaryFile("w", suffix=".log", delete=False)
    _TEMP_LOGS.append(handle.name)
    for line in lines:
        handle.write(line)
    handle.close()
    return handle.name


def tearDownModule():
    for path in _TEMP_LOGS:
        try:
            os.unlink(path)
        except OSError:
            pass
    _TEMP_LOGS.clear()


if __name__ == "__main__":
    unittest.main(verbosity=2)