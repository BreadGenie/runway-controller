"""Exercise secret transport at the actual guest-script boundary."""
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

SCRIPT = Path(__file__).with_name("guest-runner.sh")


@unittest.skipIf(os.getuid() == 0, "guest runner deliberately rejects root")
class GuestRunnerTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.directory = Path(self.tmp.name)
        self.runner = self.directory / "run.sh"
        self.runner.write_text(
            '#!/usr/bin/env bash\n'
            '[[ ! -e jit && $# == 0 ]] || exit 9\n'
            '[[ "$ACTIONS_RUNNER_INPUT_JITCONFIG" == fake-test-secret ]] || exit 10\n'
            'exit 17\n'
        )
        self.runner.chmod(0o700)
        self.secret = self.directory / "jit"
        self.secret.write_text("fake-test-secret\n")
        self.secret.chmod(0o600)

    def invoke(self, secret=None):
        return subprocess.run(
            ["bash", str(SCRIPT), str(self.directory), str(secret or self.secret)],
            capture_output=True, text=True,
        )

    def test_consumes_secret_and_preserves_runner_exit(self):
        result = self.invoke()
        self.assertEqual(result.returncode, 17, result.stderr)
        self.assertFalse(self.secret.exists())
        self.assertNotIn("fake-test-secret", result.stdout + result.stderr)

    def test_rejects_world_readable_secret(self):
        self.secret.chmod(0o644)
        result = self.invoke()
        self.assertEqual(result.returncode, 1)
        self.assertTrue(self.secret.exists())

    def test_rejects_symlink(self):
        link = self.directory / "link"
        link.symlink_to(self.secret)
        self.assertEqual(self.invoke(link).returncode, 1)
        self.assertTrue(self.secret.exists())


if __name__ == "__main__":
    unittest.main()
