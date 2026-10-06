"""Tests for scripts/validate_bench_manifest.py.

The guard exists to catch an image whose baked bench is not the bench its manifest
claims, so each case here is a way that claim could be wrong. The fixtures build a real
bench-shaped tree with a real git checkout, because the validator resolves the app commit
itself and would pass a fake one for the wrong reason.
"""
import contextlib
import importlib.util
import io
import json
import os
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

REPO = Path(__file__).resolve().parent.parent
VALIDATOR = REPO / "scripts" / "validate_bench_manifest.py"

spec = importlib.util.spec_from_file_location("validate_bench_manifest", VALIDATOR)
validator = importlib.util.module_from_spec(spec)
spec.loader.exec_module(validator)

COMMIT = "4" * 40
SIZES = {"app": 2, "assets": 1, "env": 1, "node_modules": 1}


def build_bench(root, sizes=None, **overrides):
    """Create a bench tree the validator accepts, and return its manifest path."""
    app = root / "apps" / "frappe"
    app.mkdir(parents=True)
    (root / "env").mkdir()
    (root / "sites" / "assets").mkdir(parents=True)
    (app / "node_modules").mkdir()

    # Small recorded sizes are enough: the validator's tolerance is generous by
    # design, so the accept case cannot fail on block rounding. The reject case uses a
    # number far outside any tolerance.
    recorded = dict(sizes or SIZES)
    for target, key in ((app, "app"), (root / "env", "env"),
                        (app / "node_modules", "node_modules"),
                        (root / "sites" / "assets", "assets")):
        (target / "payload").write_bytes(b"\0" * (recorded[key] * 1024 * 1024))

    env = {"GIT_AUTHOR_NAME": "t", "GIT_AUTHOR_EMAIL": "t@example.invalid",
           "GIT_COMMITTER_NAME": "t", "GIT_COMMITTER_EMAIL": "t@example.invalid",
           "PATH": os.environ.get("PATH", "")}
    subprocess.run(["git", "init", "-q", "-b", "bake", str(app)], check=True, env=env)
    subprocess.run(["git", "-C", str(app), "add", "-A"], check=True, env=env)
    subprocess.run(["git", "-C", str(app), "commit", "-q", "-m", "baked"], check=True, env=env)
    # The manifest records the commit this checkout really carries, because the
    # validator resolves HEAD itself and would pass a fixture for the wrong reason.
    head = subprocess.run(["git", "-C", str(app), "rev-parse", "HEAD"], check=True,
                          capture_output=True, text=True, env=env).stdout.strip()

    manifest = {
        "bench_root": str(root),
        "frappe_commit": overrides.pop("frappe_commit", head),
        "frappe_ref": head,
        "frappe_repository": "https://github.com/example/frappe.git",
        "node_version": "24.21.0",
        "python_version": "3.14.7",
        "size_mib": dict(sizes or SIZES),
    }
    manifest.update(overrides)
    path = root / "bench.json"
    path.write_text(json.dumps(manifest, indent=2))
    return path


class ValidateBenchManifestTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name) / "bench"
        self.root.mkdir()

    def assertRejected(self, manifest_path, *fragments):
        # fail() reports to stderr and raises SystemExit, so the reason has to be read
        # from the stream rather than from the exception.
        stream = io.StringIO()
        with contextlib.redirect_stderr(stream):
            with self.assertRaises(SystemExit) as raised:
                validator.validate(manifest_path)
        self.assertNotEqual(raised.exception.code, 0)
        for fragment in fragments:
            self.assertIn(fragment, stream.getvalue())

    def test_accepts_a_bench_that_matches_its_manifest(self):
        manifest = build_bench(self.root)
        validator.validate(manifest)

    def test_rejects_a_manifest_that_claims_another_commit(self):
        manifest = build_bench(self.root, frappe_commit="5" * 40)
        self.assertRejected(manifest, "claims the app at")

    def test_rejects_an_unexpected_key(self):
        # The exact key set is what stops a credential riding into a shared artifact.
        manifest = build_bench(self.root, credential="hunter2")
        self.assertRejected(manifest, "declares keys", "credential")

    def test_rejects_a_missing_key(self):
        manifest = build_bench(self.root)
        data = json.loads(manifest.read_text())
        del data["node_version"]
        manifest.write_text(json.dumps(data))
        self.assertRejected(manifest, "declares keys")

    def test_rejects_a_size_the_filesystem_disagrees_with(self):
        manifest = build_bench(self.root)
        data = json.loads(manifest.read_text())
        data["size_mib"]["env"] = 9999
        manifest.write_text(json.dumps(data))
        self.assertRejected(manifest, "records env at", "env measures")

    def test_rejects_a_tree_that_is_not_there(self):
        manifest = build_bench(self.root)
        (self.root / "sites" / "assets" / "payload").unlink()
        (self.root / "sites" / "assets").rmdir()
        self.assertRejected(manifest, "does not exist")

    def test_rejects_a_bench_root_that_is_absent(self):
        manifest = build_bench(self.root)
        data = json.loads(manifest.read_text())
        data["bench_root"] = str(self.root / "elsewhere")
        manifest.write_text(json.dumps(data))
        self.assertRejected(manifest, "is not a directory")

    def test_rejects_a_malformed_commit(self):
        manifest = build_bench(self.root, frappe_commit="not-a-sha")
        self.assertRejected(manifest, "which is not what this sealer accepts")

    def test_rejects_a_non_github_repository(self):
        manifest = build_bench(self.root, frappe_repository="https://example.invalid/x.git")
        self.assertRejected(manifest, "which is not what this sealer accepts")

    def test_rejects_a_non_json_manifest(self):
        manifest = self.root / "bench.json"
        manifest.write_text("{not json")
        self.assertRejected(manifest, "not readable JSON")

    def test_rejects_a_manifest_that_is_not_an_object(self):
        manifest = self.root / "bench.json"
        manifest.write_text("[]")
        self.assertRejected(manifest, "not a JSON object")

    def test_rejects_a_negative_size(self):
        manifest = build_bench(self.root)
        data = json.loads(manifest.read_text())
        data["size_mib"]["assets"] = 0
        manifest.write_text(json.dumps(data))
        self.assertRejected(manifest, "not a positive size")

    def test_command_line_requires_a_manifest_argument(self):
        self.assertEqual(subprocess.run([sys.executable, str(VALIDATOR)],
                                        capture_output=True).returncode, 2)


if __name__ == "__main__":
    unittest.main()