#!/usr/bin/env python3
"""Pinned client regression through the real guest relay and a Go TLS fixture.

Run with RUNWAY_CACHE_CLIENT_TEST=1 go test ./cache/transport -run TestPinnedClientsThroughRelay.
The Go test owns the authenticated-transport fixture. This process owns the guest
relay; TCP substitutes only for its vsock dial. Bundles are verified offline.
"""

import argparse
import importlib.machinery
import importlib.util
import os
from pathlib import Path
import shutil
import socket
import tempfile
import threading

import pinned_clients as cache
import pinned_clients as setup


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--transport-port", type=int, required=True)
    args = parser.parse_args()
    node = os.environ.get("NODE_BIN", cache.NODE_DEFAULT)
    require(not cache.check_tools(node), "Node/tar/zstd prerequisite missing")
    require(bool(setup.YARN_BIN), "Yarn prerequisite missing")
    cache_bundles = cache.fetch_bundles("cache")
    setup_bundles = setup.fetch_bundles("setup-node")
    relay_path = Path(__file__).parents[1] / "actions-runner" / "cache-relay"
    loader = importlib.machinery.SourceFileLoader("cache_relay", str(relay_path))
    spec = importlib.util.spec_from_loader(loader.name, loader)
    relay = importlib.util.module_from_spec(spec)
    loader.exec_module(relay)
    listener = socket.socket()
    listener.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    listener.bind(relay.ADDRESS)
    listener.listen(relay.CONNECTIONS)
    stop = threading.Event()
    worker = threading.Thread(target=relay.serve, args=(
        listener, stop, lambda: socket.create_connection(("127.0.0.1", args.transport_port), timeout=2)))
    worker.start()
    try:
        with tempfile.TemporaryDirectory(prefix="runway-relay-clients-") as directory:
            root = Path(directory)
            # Enforce the client's network boundary without changing bundle bytes.
            guard = root / "network-guard.cjs"
            guard.write_text('''const net = require('net');
const tls = require('tls');
const fs = require('fs');
const connect = net.Socket.prototype.connect;
net.Socket.prototype.connect = function(...args) {
  const options = Array.isArray(args[0]) ? args[0][0] : args[0];
  const allowed = options && typeof options === 'object' &&
    options.host === '127.0.0.1' && Number(options.port) === 18080;
  fs.appendFileSync(process.env.RUNWAY_NETWORK_AUDIT, allowed ? 'relay\\n' : 'blocked\\n');
  if (!allowed) throw new Error('client connection outside guest relay');
  return connect.apply(this, args);
};
tls.connect = function() {
  fs.appendFileSync(process.env.RUNWAY_NETWORK_AUDIT, 'blocked\\n');
  throw new Error('client TLS connection outside guest relay');
};
''')
            audit = root / "network-audit"
            os.environ["NODE_OPTIONS"] = "--require=" + str(guard)
            os.environ["RUNWAY_NETWORK_AUDIT"] = str(audit)
            for variable in ("HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"):
                os.environ.pop(variable, None)
            os.environ["NO_PROXY"] = "127.0.0.1"
            for kind, bundles in (("cache", cache_bundles), ("setup-node", setup_bundles)):
                workspace = root / kind / "workspace"
                workspace.mkdir(parents=True)
                client = cache.Client(bundles, node, "http://127.0.0.1:18080/")
                extra = {"RUNNER_OS": "Linux", "CACHE_UPLOAD_CONCURRENCY": "1", "CACHE_UPLOAD_CHUNK_SIZE": "0.5"}
                if kind == "cache":
                    content = workspace / "content"
                    content.mkdir()
                    inputs = {"path": str(content), "key": "relay-cache-key", "upload-chunk-size": "524288"}
                    restore, save = "restore", "save"
                else:
                    (workspace / "package.json").write_text('{"name":"relay-fixture","private":true}')
                    (workspace / "yarn.lock").write_text('# yarn lockfile v1\n')
                    _, _, content_path = setup.yarn_probe(str(workspace))
                    content = Path(content_path)
                    content.mkdir(parents=True, exist_ok=True)
                    inputs = {"cache": "yarn"}
                    restore, save = "setup", "cache-save"
                (content / "archive.bin").write_bytes(os.urandom(2 * 1024 * 1024))
                expected = cache.file_tree_hashes(str(content))
                rc, output, outputs, state = client.run_action(restore, inputs, "github-secret", str(workspace), extra_env=extra, timeout=30)
                require(rc == 0 and outputs.get("cache-hit") != "true" and state.get("CACHE_KEY"), kind + " miss failed: " + output)
                post = dict(extra, **{"STATE_" + key: value for key, value in state.items()})
                rc, output, _, _ = client.run_action(save, inputs, "github-secret", str(workspace), extra_env=post, timeout=30)
                require(rc == 0 and "Cache saved with" in output, kind + " save failed: " + output)
                shutil.rmtree(content)
                content.mkdir()
                rc, output, outputs, _ = client.run_action(restore, inputs, "github-secret", str(workspace), extra_env=extra, timeout=30)
                require(rc == 0 and outputs.get("cache-hit") == "true", kind + " restore failed: " + output)
                require(cache.file_tree_hashes(str(content)) == expected, kind + " restored bytes changed")
                if kind == "cache":
                    shutil.rmtree(content)
                    content.mkdir()
                    fallback = dict(inputs, key="relay-cache-new", **{"restore-keys": "relay-cache-"})
                    rc, output, outputs, _ = client.run_action(restore, fallback, "github-secret", str(workspace), extra_env=extra, timeout=30)
                    require(rc == 0 and outputs.get("cache-hit") == "false", "restore-key did not report a partial hit: " + output)
                    require(cache.file_tree_hashes(str(content)) == expected, "restore-key bytes changed")
                print(kind + ": miss, save, relay restore and byte equality passed", flush=True)
            connections = audit.read_text().splitlines()
            require(connections and set(connections) == {"relay"}, "client attempted a direct service connection")
            print("client connections reached only the fixed guest relay", flush=True)
    finally:
        stop.set()
        worker.join(5)
        require(not worker.is_alive(), "relay worker survived cleanup")


if __name__ == "__main__":
    main()
