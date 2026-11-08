# Runner regional cache integration

See the [standalone spec](../../specs/runway-regional-cache.md).
The static `cmd/cache-node` wrapper replaces the guest runner's external node20
and node24 executables; their original binaries remain beside them as node.real.
The guest runs the stock, checksum-pinned Actions runner from the Metal image;
no runner patch or Runner.Worker build is needed.

Supported bundle identities:

- actions/cache v4.3.0 (`0057852bfaa89a56745cba8c7296529d2fc39830`): restore, save,
  restore-only and save-only bundles.
- actions/cache v5.1.0 (`caa296126883cff596d87d8935842f9db880ef25`): restore, save,
  restore-only and save-only bundles.
- actions/cache v6.1.0 (`55cc8345863c7cc4c66a329aec7e433d2d1c52a9`): restore, save,
  restore-only and save-only bundles.
- actions/setup-node v6.5.0 (`249970729cb0ef3589644e2896645e5dc5ba9c38`): setup and
  cache-save bundles.
- actions/setup-node v7.1.0 (`949feb2413d6458794dcd2491c4babbbce0c15c1`): setup and
  cache-save bundles.

The manifests have no pre entrypoints. Routing requires both the matching
_actions/actions directory and a pinned SHA256 of the script bytes. Mutable tags
cannot authorize different content. Unrelated action scripts exec immediately,
without filesystem reads or hashing. Workflow shell steps do not use externals.
Container executables under /__e pass through; containers also lack the guest's
/run/runway directory and vsock setup infrastructure.

The first supported bundle reads GITHUB_* context hints and exchanges one bounded
JSON line with vsock 1026. Wire version 2 sends version, nonce and context only;
the host resolves job UUID and request ID for the bound runner from host-held
scale-set and REST records, and verifies context against GitHub REST. The response echoes version and nonce,
with outcome and the fixed credential-free route http://127.0.0.1:18080/.
Setup probes the relay for at most 0.2s, connects vsock within 1s and has a total
4s budget including lock wait. Errors keep GitHub caching; setup never retries.

An atomic enabled/disabled file under /run/runway is keyed by run ID, attempt and
job name. Flock serializes setup, so main/post share the decision. Enabled scripts
replace ACTIONS_CACHE_URL and remove ACTIONS_CACHE_SERVICE_V2; all other env,
arguments and argv[0] survive exec. Artifact inputs and tokens remain untouched.
Unsupported bundles and contexts retain GitHub caching.
A script under `_actions/actions/{cache,setup-node}/` that is not allowlisted
(for example after a moved `@v4`/`@v6` tag) writes one line to its step log:
`runway-cache: actions/NAME bundle sha256:PREFIX (FILE) is not on the
regional-cache allowlist; this step uses GitHub's cache`. cache-host's `stopped`
event records `setup_enabled`. All supported releases passed the offline pinned-client
tests through the relay using cache protocol v1. setup-node v7.1.0 is tested
with `ACTIONS_CACHE_MODE` unset, matching the runner environment. Future releases
need their hashes and a passing pinned-client run before allowlisting.

cache-relay forwards bounded connections to vsock 1027. Its 330-second idle
limit covers large-save finalization. The installer stages the static wrapper,
relay and systemd unit; the launcher installs them into each guest clone.
Existing node.real files are preserved on repeated installation.

Build with `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o cache-node ./cmd/cache-node`
and pass that binary as install.sh's second argument. Deploy host and guest
together: wire v1 is unsupported. Prepare the guest image from an unregistered
stock Metal runner image; image preparation never swaps Runner.Worker.

Standalone guests run through `/usr/bin/jailer` in the existing systemd unit,
using slot accounts `runway-fc-1` through `runway-fc-9`. Each jail lives at
`/srv/runway-jail/firecracker/VM/root`, with a kernel hardlink (copy across
filesystems), its own root-drive device node, and jail-relative config paths
`/vmlinux`, `/dev/rootfs` and `/vsock/v.sock`. The VM socket directory is
bind-mounted at `vsock/`, owned by root and the slot group with mode `3770`
(setgid, so new sockets get the slot group, and sticky, so the jailed Firecracker
can create its own `v.sock` but cannot remove cache-host's);
cache-host sockets use that group and mode `0660`. Binding v2 records
`jail_uid` and `jail_gid`: cache-host requires the peer PID, uid and gid to
match, and the gid must also match the socket directory. V1 bindings are refused.
The launcher verifies the jail's executable path and its digest against
`/usr/bin/firecracker` before writing the binding. Teardown, reap and failed
launch cleanup stop the unit, unmount `vsock/`, then remove the jail with
`rm -rf --one-file-system`. No extra PID namespace or jailer cgroup is used.

`deploy/blr-garage/install.sh` creates the nine system accounts (nologin, no
home) and the root-owned `0711` jail base. It refuses installation while any
binding file exists in `/run/runway-cache/bindings`; tear down guests first.
Rollback requires restoring both the launcher and runway `.previous` binaries.


The routing source of truth is `cmd/cache-node/allowlist.json`: repository,
release, full commit, entrypoint and SHA256. setup-node's action.yml is
harness-only. cache-node embeds the file; invalid data disables all regional
routing without preventing Node execution. Non-candidates still perform no file I/O.

The daily and manually dispatched `Cache allowlist` workflow tracks every major
tag at or above the oldest pinned major, including newly published majors. It
ignores releases without a major tag, retains every tested pin, resolves manifests
and fetches bundles by full commit. It runs vet, race tests and every pinned relay
scenario before opening an idempotent `bot/cache-allowlist-<versions>` PR. Failures
appear in the run summary and produce no PR. Review and merge the PR, then rebuild
and deploy the wrapper through the normal deployment process.

Manual fallback from the repository root:

```sh
python3 -I integrations/actions-cache/update_allowlist.py --dry-run
python3 -I integrations/actions-cache/update_allowlist.py --fetch-dir /tmp/runway-cache-clients
RUNWAY_CACHE_VENDOR=/tmp/runway-cache-clients RUNWAY_CACHE_CLIENT_TEST=1 go test -race ./cache/transport -run TestPinnedClientsThroughRelay -v
```

The update command appends candidates; review its JSON diff and run all workflow
checks before committing. `--fetch-dir` provisions hash-checked files into a
chosen cache directory. Tests remain offline and use `RUNWAY_CACHE_VENDOR`, or
the existing gitignored `.scratch/runway-regional-cache/vendor/` by default.
Node, Yarn classic and zstd are required.

Enable repository Settings → Actions → General → Workflow permissions →
**Allow GitHub Actions to create and approve pull requests** for automated PRs.
The workflow grants contents/write and pull-requests/write only to its update
job and uses the automatic GITHUB_TOKEN; no additional secret is required.
Organization policy must permit these permissions. PRs created with GITHUB_TOKEN
do not trigger ordinary pull_request workflows; the bot runs its checks before
creating the PR. Required external checks may need a human-triggered run.
