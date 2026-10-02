"""Execute the workflow's actual Python heredoc against a socket fixture."""
from pathlib import Path
import socket
import textwrap
import threading
import unittest
from unittest.mock import patch


class SmokeWorkflowTest(unittest.TestCase):
    def test_embedded_probe_sends_valid_redis_resp(self):
        workflow = Path(__file__).with_name("smoke-workflow.yml").read_text()
        body = workflow.split("python3 - <<'PY'\n", 1)[1].split("          PY", 1)[0]
        script = compile(textwrap.dedent(body), "workflow Redis probe", "exec")
        client, server = socket.socketpair()
        received = []

        def redis_fixture():
            with server:
                server.settimeout(3)
                received.append(server.recv(1024))
                server.sendall(b"+PONG\r\n")

        worker = threading.Thread(target=redis_fixture)
        worker.start()
        try:
            with patch("socket.create_connection", return_value=client) as connect:
                exec(script, {})
                connect.assert_called_once_with(("127.0.0.1", 6379), timeout=10)
        finally:
            client.close()
            worker.join(timeout=5)
        self.assertFalse(worker.is_alive())
        self.assertEqual(received, [b"*1\r\n$4\r\nPING\r\n"])
        self.assertNotIn(b"\\r", received[0])


if __name__ == "__main__":
    unittest.main()
