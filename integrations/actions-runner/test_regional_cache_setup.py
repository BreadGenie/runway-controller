"""Real socket-pair protocol coverage; no VM or host receiver required."""
import copy
import importlib.machinery
import importlib.util
import io
import json
from pathlib import Path
import socket
import threading
import unittest
from unittest import mock

loader = importlib.machinery.SourceFileLoader("cache_setup", str(Path(__file__).with_name("cache-setup")))
spec = importlib.util.spec_from_loader(loader.name, loader)
helper = importlib.util.module_from_spec(spec)
loader.exec_module(helper)


def request():
    return dict(version=1, job_id="328d267e-5e8a-42c9-8f85-b63842874f58", request_id=0,
                nonce="328d267e-5e8a-42c9-8f85-b63842874f59", context={key: None for key in helper.CONTEXT})


class CacheSetupTests(unittest.TestCase):
    def exchange(self, response):
        client, server = socket.socketpair()
        observed = []
        def receive():
            with server:
                with server.makefile("rb") as stream:
                    observed.append(json.loads(stream.readline(32769)))
                try:
                    server.sendall(response + b"\n")
                except BrokenPipeError:
                    pass
        thread = threading.Thread(target=receive)
        thread.start()
        try:
            with client:
                return helper.exchange(client, request())
        finally:
            thread.join(4)
            self.assertFalse(thread.is_alive())
            self.assertEqual(observed, [request()])

    def test_newline_exchange_needs_no_half_close_or_eof(self):
        client, server = socket.socketpair()
        done = threading.Event()
        failures = []
        response = helper.disabled(request())
        response.update(outcome="enabled", route=helper.ROUTE)
        def receive():
            try:
                with server, server.makefile("rb") as stream:
                    server.settimeout(1)
                    self.assertEqual(json.loads(stream.readline(32769)), request())
                    server.sendall(json.dumps(response).encode() + b"\n")
                    self.assertTrue(done.wait(1), "helper waited for response EOF")
            except BaseException as error:
                failures.append(error)
        thread = threading.Thread(target=receive)
        thread.start()
        try:
            with client:
                self.assertEqual(helper.exchange(client, request()), response)
                done.set()
        finally:
            done.set()
            thread.join(2)
        self.assertFalse(failures, failures)
        self.assertFalse(thread.is_alive())

    def test_enabled_and_disabled_echo_accept_zero_request(self):
        for outcome, route in [("enabled", helper.ROUTE), ("disabled", None)]:
            with self.subTest(outcome=outcome):
                response = helper.disabled(request())
                response.update(outcome=outcome, route=route)
                self.assertEqual(self.exchange(json.dumps(response).encode()), response)

    def test_strict_identity_route_and_fields(self):
        for field, value in [("nonce", "bad"), ("job_id", "bad"), ("request_id", 1),
                             ("version", True), ("extra", "secret"), ("route", "https://evil/"),
                             ("outcome", "enabled")]:
            with self.subTest(field=field):
                response = helper.disabled(request())
                response[field] = value
                with self.assertRaises(ValueError):
                    self.exchange(json.dumps(response).encode())

    def test_duplicate_malformed_oversize(self):
        for data in [b'{}', b'bad', b'x'*4097, b'{"version":1,"version":1}', b'[]', b'{} {}', b'{}\n{}']:
            with self.subTest(data=data[:30]), self.assertRaises((ValueError, TypeError)):
                self.exchange(data)

    def test_request_has_no_token_env_input_or_nested_data(self):
        for mutation in [lambda r: r.update(token="secret"), lambda r: r.update(env={}),
                         lambda r: r["context"].update(token="secret"),
                         lambda r: r["context"].update(ref={"nested": 1}),
                         lambda r: r.update(request_id=-1), lambda r: r.update(request_id=2**63),
                         lambda r: r.update(request_id=True)]:
            value = copy.deepcopy(request())
            mutation(value)
            with self.assertRaises(ValueError):
                helper.validate_request(value)
        for value in [0, 2**63-1]:
            valid = request()
            valid["request_id"] = value
            helper.validate_request(valid)

    def test_unfinished_receiver_returns_explicit_disabled(self):
        class Input:
            buffer = io.BytesIO(json.dumps(request()).encode())
        output = io.StringIO()
        with mock.patch.object(helper.sys, "stdin", Input()), mock.patch.object(helper.sys, "stdout", output), mock.patch.object(helper.socket, "socket", side_effect=OSError("unavailable")):
            self.assertEqual(helper.main(), 0)
        self.assertEqual(json.loads(output.getvalue()), helper.disabled(request()))

    def test_missing_relay_disables_without_any_vsock_call(self):
        class Input:
            buffer = io.BytesIO(json.dumps(request()).encode())
        probe = mock.MagicMock()
        probe.__enter__.return_value = probe
        probe.connect.side_effect = ConnectionRefusedError()
        output = io.StringIO()
        with mock.patch.object(helper.sys, "stdin", Input()), mock.patch.object(helper.sys, "stdout", output), mock.patch.object(helper.socket, "socket", return_value=probe):
            self.assertEqual(helper.main(), 0)
        self.assertEqual(json.loads(output.getvalue()), helper.disabled(request()))

    def connect(address):
            probe.__exit__.assert_called_once()
            self.assertEqual(address, (socket.VMADDR_CID_HOST, 1026))
        native.connect.side_effect = connect
        output = io.StringIO()
        with mock.patch.object(helper.sys, "stdin", Input()), mock.patch.object(helper.sys, "stdout", output), mock.patch.object(helper.socket, "socket", side_effect=[probe, native]):
            self.assertEqual(helper.main(), 0)
        self.assertEqual(factory.call_args_list, [mock.call(socket.AF_INET, socket.SOCK_STREAM), mock.call(socket.AF_VSOCK, socket.SOCK_STREAM)])
        self.assertEqual(json.loads(native.sendall.call_args.args[0]), request())
        self.assertEqual(json.loads(output.getvalue()), response)

    def test_duplicate_input_and_size(self):
        for data in [b'{"version":1,"version":1}', b'x'*32769]:
            with self.assertRaises(ValueError):
                helper.read_object(io.BytesIO(data), 32768)


if __name__ == "__main__":
    unittest.main()
