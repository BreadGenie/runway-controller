import json
from pathlib import Path
import tempfile
import unittest
import update_allowlist as bot


class UpdateTests(unittest.TestCase):
    def setUp(self):
        self.sha = "a" * 40
        self.pins = [{"repo": r, "version": v, "commit": "b" * 40}
                     for r, v in (("actions/cache", "v4.3.0"), ("actions/setup-node", "v6.5.0"), ("astral-sh/setup-uv", "v6.8.0"))]

    def test_major_targets_only_and_new_major(self):
        tags = [{"name": n, "commit": {"sha": self.sha}} for n in ("v3", "v8", "v8.1.0")]
        tags.append({"name": "v9.0.0", "commit": {"sha": "c" * 40}})
        found = bot.candidates(self.pins, lambda url: json.dumps(tags).encode())
        self.assertEqual(found, [(r, "v8.1.0", self.sha) for r in ("actions/cache", "actions/setup-node", "astral-sh/setup-uv")])

    def test_pinned_target_is_noop(self):
        tags = [{"name": "v6", "commit": {"sha": "b" * 40}}]
        self.assertEqual(bot.candidates(self.pins, lambda url: json.dumps(tags).encode()), [])

    def test_manifest_relative_and_rejection(self):
        self.assertEqual(bot.manifest_paths(b"runs:\n  main: '../dist/restore-only/index.js'\n", "restore/"),
                         ["dist/restore-only/index.js"])
        for data in (b"runs: {}", b"  pre: dist/pre/index.js", b"  main: ../../escape"):
            with self.assertRaises(ValueError):
                bot.manifest_paths(data)

    def test_resolve_and_hash_checked_provision(self):
        manifest = b"runs:\n  main: dist/setup/index.js\n  post: dist/cache-save/index.js\n"
        get = lambda url: manifest if url.endswith("action.yml") else b"bundle"
        entries = bot.resolve(("actions/setup-node", "v8.0.0", self.sha), get)
        self.assertEqual(len(entries), 3)
        with tempfile.TemporaryDirectory() as directory:
            bot.provision(entries, Path(directory), get)
            bot.provision(entries, Path(directory), lambda url: self.fail("cached bytes fetched"))
            with self.assertRaises(ValueError):
                bot.provision(entries, Path(directory) / "bad", lambda url: b"bad")

    def test_tag_pagination(self):
        calls = []
        def get(url):
            calls.append(url)
            batch = [{"name": "v1.0.0", "commit": {"sha": self.sha}}] * 100 if url.endswith("&page=1") else []
            return json.dumps(batch).encode()
        self.assertEqual(len(bot.tags("actions/cache", get)), 100)
        self.assertEqual(len(calls), 2)

    def test_unresolved_release_fails_closed(self):
        tags = [{"name": "v8", "commit": {"sha": self.sha}}]
        with self.assertRaises(ValueError):
            bot.candidates(self.pins, lambda url: json.dumps(tags).encode())

    def test_cache_manifests(self):
        def get(url):
            if url.endswith("/restore/action.yml"):
                return b"  main: ../dist/restore-only/index.js\n"
            if url.endswith("/save/action.yml"):
                return b"  main: ../dist/save-only/index.js\n"
            if url.endswith("/action.yml"):
                return b"  main: dist/restore/index.js\n  post: dist/save/index.js\n"
            return b"bundle"
        entries = bot.resolve(("actions/cache", "v8.0.0", self.sha), get)
        self.assertEqual(len(entries), 4)
        self.assertFalse(any(e["harness_only"] for e in entries))

    def test_setup_uv_manifests_and_provision(self):
        for version, ext in (("v6.8.0", "js"), ("v7.6.0", "cjs")):
            manifest = f'runs:\n  main: "dist/setup/index.{ext}"\n  post: "dist/save-cache/index.{ext}"\n'.encode()
            get = lambda url: manifest if url.endswith("action.yml") else b"uv bundle"
            entries = bot.resolve(("astral-sh/setup-uv", version, self.sha), get)
            self.assertEqual(len(entries), 3)
            with tempfile.TemporaryDirectory() as directory:
                bot.provision(entries, Path(directory), get)
                self.assertTrue((Path(directory) / ("setup-uv-" + self.sha[:8]) / ("save-cache-index." + ext)).exists())
            with self.assertRaises(ValueError):
                bot.resolve(("astral-sh/setup-uv", version, self.sha),
                            lambda url: b"  main: dist/unknown/index.js\n" if url.endswith("action.yml") else b"bundle")
