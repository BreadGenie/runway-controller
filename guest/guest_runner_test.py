"""Exercise secret transport at the actual guest-script boundary."""
import os
from pathlib import Path
import shlex
import subprocess
import tempfile
import unittest

SCRIPT = Path(__file__).with_name("guest-runner.sh")
BAKED_TOOL_CACHE = Path("/opt/hostedtoolcache")
UNSET = "<unset>"


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

    def invoke(self, secret=None, env=None):
        return subprocess.run(
            ["bash", str(SCRIPT), str(self.directory), str(secret or self.secret)],
            capture_output=True, text=True,
            env=env,
        )

    def install_probe_runner(self):
        """Replace run.sh with one that records the tool cache it was handed.

        Uses ${VAR-default} rather than ${VAR:-default} so an empty value is
        reported as empty and stays distinguishable from an unset variable.
        """
        probe = self.directory / "toolcache.seen"
        self.runner.write_text(
            '#!/usr/bin/env bash\n'
            '[[ ! -e jit && $# == 0 ]] || exit 9\n'
            f'printf %s "${{RUNNER_TOOL_CACHE-{UNSET}}}" > {shlex.quote(str(probe))}\n'
            'exit 17\n'
        )
        self.runner.chmod(0o700)
        return probe

    def observed_tool_cache(self, env):
        probe = self.install_probe_runner()
        result = self.invoke(env=env)
        self.assertEqual(result.returncode, 17, result.stderr)
        return probe.read_text()

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

    def test_preserves_explicit_tool_cache(self):
        """An operator-supplied tool cache wins, even if the baked one exists.

        This discriminates only where /opt/hostedtoolcache actually exists: on a
        host without the prepared directory the baked branch is inert, so an
        implementation that ignored the inherited value would also pass here.
        The other two tests cover the branch wherever the directory is absent.
        """
        explicit = self.directory / "explicit-toolcache"
        explicit.mkdir()
        env = dict(os.environ, RUNNER_TOOL_CACHE=str(explicit))
        self.assertEqual(self.observed_tool_cache(env), str(explicit))

    @unittest.skipIf(
        BAKED_TOOL_CACHE.is_dir(), "this host provides a baked tool cache"
    )
    def test_does_not_force_tool_cache_without_prepared_directory(self):
        """A bare image keeps the runner's own <work>/_tool default."""
        env = {k: v for k, v in os.environ.items() if k != "RUNNER_TOOL_CACHE"}
        self.assertEqual(self.observed_tool_cache(env), UNSET)

    @unittest.skipIf(
        BAKED_TOOL_CACHE.is_dir(), "this host provides a baked tool cache"
    )
    def test_empty_tool_cache_does_not_force_prepared_directory(self):
        """An empty value is not a usable override and must not adopt the baked path."""
        env = dict(os.environ, RUNNER_TOOL_CACHE="")
        self.assertEqual(self.observed_tool_cache(env), "")


if __name__ == "__main__":
    unittest.main()
