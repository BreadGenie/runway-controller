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
import json
import contextlib
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
    parser.add_argument("--transport-port", type=int)
    parser.add_argument("--client")
    parser.add_argument("--fallback-url", help="expect cache-node to retain this GitHub cache substitute")
    parser.add_argument("--artifacts", help="retain guest-visible files for parent secret audit")
    parser.add_argument("--managed-context", help="setup context for the cache-node subprocess")
    parser.add_argument("--node-wrapper", help="test cache-node executable shim")
    parser.add_argument("--tls-ca", help="use the real mesh TLS connector with this test CA")
    parser.add_argument("--list-clients", action="store_true")
    args = parser.parse_args()
    if args.list_clients:
        print("\n".join(case[0] for case in cache.client_cases()))
        return
    require(args.transport_port is not None, "--transport-port is required")
    node = os.environ.get("NODE_BIN", cache.NODE_DEFAULT)
    require(not cache.check_tools(node), "Node/tar/zstd prerequisite missing")
    require(bool(setup.YARN_BIN), "Yarn prerequisite missing")
    selected = [case for case in cache.client_cases() if case[0] == args.client]
    require(len(selected) == 1, "unknown pinned client: " + args.client)
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
    connect = (relay.tls_connector("127.0.0.1", args.transport_port, Path(args.tls_ca).read_bytes())
               if args.tls_ca else
               lambda: socket.create_connection(("127.0.0.1", args.transport_port), timeout=2))
    worker = threading.Thread(target=relay.serve, args=(listener, stop, connect))
    worker.start()
    try:
        with (contextlib.nullcontext(args.artifacts) if args.artifacts else
              tempfile.TemporaryDirectory(prefix="runway-relay-clients-")) as directory:
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
    options.host === '127.0.0.1' && Number(options.port) === Number(process.env.RUNWAY_ALLOWED_PORT || 18080);
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
            if args.fallback_url:
                from urllib.parse import urlparse
                os.environ["RUNWAY_ALLOWED_PORT"] = str(urlparse(args.fallback_url).port)
            for variable in ("HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"):
                os.environ.pop(variable, None)
            os.environ["NO_PROXY"] = "127.0.0.1"
            for label, kind, version, mode in selected:
                bundles = cache.fetch_bundles(version)
                if args.node_wrapper:
                    # Runner-shaped paths are part of the wrapper allowlist gate.
                    action_root = root / "_actions" / "actions" / kind / version["commit"]
                    action_root.mkdir(parents=True)
                    copies = {}
                    for bundle, source in bundles.items():
                        target = action_root / bundle / "index.js"
                        target.parent.mkdir(parents=True, exist_ok=True)
                        shutil.copyfile(source, target)
                        copies[bundle] = str(target)
                    bundles = copies
                workspace = root / kind / "workspace"
                workspace.mkdir(parents=True)
                upstream = args.fallback_url or ("http://github-cache.invalid/" if args.node_wrapper
                                                 else "http://127.0.0.1:18080/")
                client = cache.Client(bundles, args.node_wrapper or node, upstream)
                extra = {"RUNNER_OS": "Linux", "CACHE_UPLOAD_CONCURRENCY": "1", "CACHE_UPLOAD_CHUNK_SIZE": "0.5"}
                if args.managed_context:
                    extra.update(json.loads(Path(args.managed_context).read_text()))
                    extra["GITHUB_JOB"] = "managed-e2e"
                if kind == "cache":
                    content = workspace / "content"
                    content.mkdir()
                    inputs = {"path": str(content), "key": "relay-cache-key", "upload-chunk-size": "524288"}
                    restore, save = ("restore", "save") if mode == "main-post" else ("restore-only", "save-only")
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
                require(rc == 0 and outputs.get("cache-hit") != "true" and (state.get("CACHE_KEY") or mode == "only"), kind + " miss failed: " + output)
                if args.fallback_url:
                    print(label + ": stopped service fell back; pinned action exited successfully", flush=True)
                    continue
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
                print(label + ": miss, save, exact hit, " + ("restore-key hit, " if kind == "cache" else "") + "byte equality passed", flush=True)
            connections = audit.read_text().splitlines()
            require(connections and set(connections) == {"relay"}, "client attempted a direct service connection")
            print("client connections reached only the fixed guest relay", flush=True)
    finally:
        stop.set()
        worker.join(5)
        require(not worker.is_alive(), "relay worker survived cleanup")


if __name__ == "__main__":
    main()
