"""Exercise the one-shot VM creation boundary without infrastructure."""

import importlib.util
import json
import tempfile
import unittest
from pathlib import Path
from unittest.mock import Mock, patch

spec = importlib.util.spec_from_file_location(
    "atlas_vm", Path(__file__).with_name("atlas-vm.py")
)
vm = importlib.util.module_from_spec(spec)
spec.loader.exec_module(vm)


class CreateOnceTests(unittest.TestCase):
    def test_uncertain_create_preserves_identity_and_blocks_repeat(self):
        with tempfile.TemporaryDirectory() as directory:
            output = Path(directory) / "create.jsonl"
            client = Mock()
            client.create_vm.side_effect = TimeoutError("sensitive response")
            with self.assertRaises(TimeoutError):
                vm.create_once(
                    client,
                    {"tags": {"attempt": "attempt-1"}},
                    output,
                    {"attempt": "attempt-1"},
                )
            records = [json.loads(line) for line in output.read_text().splitlines()]
            self.assertEqual(records[0]["payload"]["tags"]["attempt"], "attempt-1")
            self.assertEqual(records[-1]["status"], "unconfirmed")
            self.assertNotIn("sensitive", output.read_text())
            self.assertEqual(output.stat().st_mode & 0o777, 0o600)
            with self.assertRaises(FileExistsError):
                vm.create_once(client, {}, output, {})
            client.create_vm.assert_called_once()

    def test_failed_durable_reservation_never_calls_atlas(self):
        with tempfile.TemporaryDirectory() as directory:
            client = Mock()
            with (
                patch.object(vm.os, "fsync", side_effect=OSError("disk failure")),
                self.assertRaises(OSError),
            ):
                vm.create_once(client, {}, Path(directory) / "create.jsonl", {})
            client.create_vm.assert_not_called()

    def test_symlink_destination_never_calls_atlas(self):
        with tempfile.TemporaryDirectory() as directory:
            output = Path(directory) / "create.jsonl"
            target = Path(directory) / "missing"
            output.symlink_to(target)
            client = Mock()
            with self.assertRaises(FileExistsError):
                vm.create_once(client, {}, output, {})
            client.create_vm.assert_not_called()
            self.assertFalse(target.exists())


if __name__ == "__main__":
    unittest.main()
