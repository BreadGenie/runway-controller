# Runner regional cache integration

See the [standalone spec](../../specs/runway-regional-cache.md).
The static `cmd/cache-node` wrapper replaces the guest runner's external node20
and node24 executables; their original binaries remain beside them as node.real.
No regional-cache runner build is needed. The separate 1025 administrative gate
still lives in the Metal image's Runner.Worker and is outside this integration.

Supported bundle identities:

- actions/cache@0057852bfaa89a56745cba8c7296529d2fc39830: restore, save,
  restore-only and save-only dist/index.js bundles (main/post).
- actions/setup-node@249970729cb0ef3589644e2896645e5dc5ba9c38: setup and
  cache-save dist/index.js bundles (main/post).

The manifests have no pre entrypoints. Routing requires both the matching
_actions/actions directory and a pinned SHA256 of the script bytes. Mutable tags
cannot authorize different content. Unrelated action scripts exec immediately,
without filesystem reads or hashing. Workflow shell steps do not use externals.
Container executables under /__e pass through; containers also lack the guest's
/run/runway directory and vsock setup infrastructure.

The first supported bundle reads GITHUB_* context hints and exchanges one bounded
JSON line with vsock 1026. Wire version 2 sends version, nonce and context only;
the host takes job UUID and request ID from its authenticated 1025 release and
verifies context against GitHub REST. The response echoes version and nonce,
with outcome and the fixed credential-free route http://127.0.0.1:18080/.
Setup probes the relay for at most 0.2s, connects vsock within 1s and has a total
4s budget including lock wait. Errors keep GitHub caching; setup never retries.

An atomic enabled/disabled file under /run/runway is keyed by run ID, attempt and
job name. Flock serializes setup, so main/post share the decision. Enabled scripts
replace ACTIONS_CACHE_URL and remove ACTIONS_CACHE_SERVICE_V2; all other env,
arguments and argv[0] survive exec. Artifact inputs and tokens remain untouched.
Unsupported bundles and contexts retain GitHub caching.

cache-relay forwards bounded connections to vsock 1027. Its 330-second idle
limit covers large-save finalization. The installer stages the static wrapper,
relay and systemd unit; the launcher installs them into each guest clone.
Existing node.real files are preserved on repeated installation.

Build with `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o cache-node ./cmd/cache-node`
and pass that binary as install.sh's second argument. Deploy host and guest
together: wire v1 is unsupported. Use an unregistered Metal runner image without
the retired regional cache changes; image preparation never swaps Runner.Worker.
