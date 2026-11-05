import importlib.machinery
import importlib.util
import http.server
from pathlib import Path
import re
import socket
import threading
import time
import unittest
import urllib.request

loader = importlib.machinery.SourceFileLoader("cache_relay", str(Path(__file__).with_name("cache-relay")))
spec = importlib.util.spec_from_loader(loader.name, loader)
relay = importlib.util.module_from_spec(spec)
loader.exec_module(relay)


class RelayTests(unittest.TestCase):
    def test_idle_outlasts_the_host_request_limit(self):
        # The host translator may be silent for its whole request limit while a
        # large finalize stores the archive; the relay must not cut it first.
        source = (Path(__file__).parents[2] / "cache_host.go").read_text()
        minutes = int(re.search(r"cacheHostRequestLimit\s*=\s*(\d+) \* time\.Minute", source).group(1))
        self.assertGreaterEqual(relay.IDLE_SECONDS, minutes * 60)

    def test_idle_clients_release_all_slots_after_first_byte_deadline(self):
        stop = threading.Event()
        listener = socket.socket()
        listener.bind(("127.0.0.1", 0))
        listener.listen()
        address = listener.getsockname()
        connected = threading.Event()
        peers = []

        def connect():
            upstream, peer = socket.socketpair()
            peers.append(peer)
            connected.set()
            return upstream

        thread = threading.Thread(target=relay.serve, args=(listener, stop, connect, 0.2))
        thread.start()
        clients = []
        try:
            self.assertEqual(relay.FIRST_BYTE_SECONDS, 5)
            for _ in range(relay.CONNECTIONS):
                clients.append(socket.create_connection(address, timeout=2))
            for client in clients:
                self.assertEqual(client.recv(1), b"")
            self.assertFalse(connected.is_set())
            with socket.create_connection(address, timeout=2) as request:
                request.sendall(b"G")
                self.assertTrue(connected.wait(1), "expired idle clients retained all relay slots")
        finally:
            stop.set()
            for client in clients:
                client.close()
            thread.join(2)
            for peer in peers:
                peer.close()
            self.assertFalse(thread.is_alive())

    def test_http_request_survives_lazy_host_connection(self):
        class Handler(http.server.BaseHTTPRequestHandler):
            def do_GET(self):
                self.send_response(204)
                self.end_headers()

            def log_message(self, *_):
                pass

        upstream = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        server = threading.Thread(target=upstream.serve_forever)
        server.start()
        stop = threading.Event()
        listener = socket.socket()
        listener.bind(("127.0.0.1", 0))
        listener.listen()
        address = listener.getsockname()
        thread = threading.Thread(target=relay.serve, args=(
            listener, stop, lambda: socket.create_connection(upstream.server_address, timeout=2)))
        thread.start()
        try:
            opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
            for _ in range(3):
                with opener.open(f"http://{address[0]}:{address[1]}/_apis/artifactcache/cache?keys=k&version=v", timeout=2) as response:
                    self.assertEqual(response.status, 204)
                    self.assertEqual(response.read(), b"")
        finally:
            stop.set()
            thread.join(2)
            upstream.shutdown()
            upstream.server_close()
            server.join(2)
            self.assertFalse(thread.is_alive())

    def test_shutdown_interrupts_idle_client_before_host_connection(self):
        stop = threading.Event()
        listener = socket.socket()
        listener.bind(("127.0.0.1", 0))
        listener.listen()
        address = listener.getsockname()
        thread = threading.Thread(target=relay.serve, args=(
            listener, stop, lambda: self.fail("idle client opened native connection")))
        thread.start()
        try:
            with socket.create_connection(address, timeout=2):
                stop.set()
                thread.join(2)
                self.assertFalse(thread.is_alive())
        finally:
            stop.set()
            thread.join(2)

    def test_empty_availability_probe_does_not_open_host_connection(self):
        stop = threading.Event()
        connected = threading.Event()
        peers = []

        def connect():
            upstream, peer = socket.socketpair()
            peers.append(peer)
            connected.set()
            return upstream

        listener = socket.socket()
        listener.bind(("127.0.0.1", 0))
        listener.listen()
        address = listener.getsockname()
        thread = threading.Thread(target=relay.serve, args=(listener, stop, connect))
        thread.start()
        try:
            with socket.create_connection(address, timeout=2):
                pass
            self.assertFalse(connected.wait(0.2), "empty probe consumed a native HTTP slot")
        finally:
            stop.set()
            thread.join(2)
            for peer in peers:
                peer.close()
            self.assertFalse(thread.is_alive())

    def test_bidirectional_bytes_and_half_close(self):
        client, downstream = socket.socketpair()
        upstream, server = socket.socketpair()
        stop = threading.Event()
        failures = []

        def forward():
            try:
                with downstream, upstream:
                    relay.relay(downstream, upstream, stop)
            except Exception as exc:
                failures.append(type(exc).__name__)

        thread = threading.Thread(target=forward)
        thread.start()
        try:
            with client, server:
                client.settimeout(2)
                server.settimeout(2)
                request = b"Authorization: fixture-only\r\n\r\n" + b"a" * (relay.BUFFER_BYTES*3)
                sender = threading.Thread(target=lambda: (client.sendall(request), client.shutdown(socket.SHUT_WR)))
                sender.start()
                received = bytearray()
                while data := server.recv(65536):
                    received.extend(data)
                sender.join(2)
                self.assertFalse(sender.is_alive())
                self.assertEqual(received, request)
                server.sendall(b"HTTP/1.1 204 No Content\r\n\r\n")
                server.shutdown(socket.SHUT_WR)
                response = bytearray()
                while data := client.recv(4096):
                    response.extend(data)
                self.assertEqual(response, b"HTTP/1.1 204 No Content\r\n\r\n")
                thread.join(2)
                self.assertFalse(thread.is_alive())
                self.assertEqual(failures, [])
        finally:
            stop.set()
            thread.join(2)

    def test_idle_and_cancellation_finish_owned_relay(self):
        for cancel in (False, True):
            with self.subTest(cancel=cancel):
                left, client = socket.socketpair()
                right, server = socket.socketpair()
                stop = threading.Event()
                with left, right, client, server:
                    started = time.monotonic()
                    thread = threading.Thread(target=relay.relay, args=(left, right, stop, 0.1 if not cancel else 10))
                    thread.start()
                    if cancel:
                        stop.set()
                    thread.join(1)
                    self.assertFalse(thread.is_alive())
                    self.assertLess(time.monotonic()-started, 1)

    def test_listener_shutdown_waits_for_owned_connection(self):
        stop = threading.Event()
        host_peer = []
        connected = threading.Event()

        def connect():
            upstream, server = socket.socketpair()
            host_peer.append(server)
            connected.set()
            return upstream

        listener = socket.socket()
        listener.bind(("127.0.0.1", 0))
        listener.listen()
        address = listener.getsockname()
        thread = threading.Thread(target=relay.serve, args=(listener, stop, connect))
        thread.start()
        try:
            with socket.create_connection(address, timeout=2) as client:
                client.sendall(b"x")
                self.assertTrue(connected.wait(2))
                stop.set()
                thread.join(2)
                self.assertFalse(thread.is_alive())
                try:
                    self.assertEqual(client.recv(1), b"")
                except ConnectionResetError:
                    pass  # Cancellation may discard the unread request byte.
        finally:
            stop.set()
            thread.join(2)
            for connection in host_peer:
                connection.close()


if __name__ == "__main__":
    unittest.main()
