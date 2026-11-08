# Regional Actions cache

Standalone deployment on blr-1 supports actions/cache v4.3.0 (`0057852bfaa89a56745cba8c7296529d2fc39830`),
v5.1.0 (`caa296126883cff596d87d8935842f9db880ef25`), v6.1.0 (`55cc8345863c7cc4c66a329aec7e433d2d1c52a9`),
and actions/setup-node v6.5.0 (`249970729cb0ef3589644e2896645e5dc5ba9c38`),
v7.1.0 (`949feb2413d6458794dcd2491c4babbbce0c15c1`), using cache protocol v1.
Other traffic keeps its route.
Only same-repository branch pushes and workflow_dispatch qualify; PRs, forks,
tags, container jobs and unsupported revisions retain GitHub caching.
A static external-Node wrapper hashes only supported action candidates and lazily
selects one job-scoped route over setup wire v2, using GITHUB_* hints verified
against GitHub REST. The guest runs the stock Actions runner with no patch;
the host resolves job identity itself.
A failure before setup also keeps GitHub caching. After setup, a failed
restore or save is a cache miss or warning, not a retry against GitHub;
fail-on-cache-miss still fails the job as it would upstream.

The root launcher binds each ephemeral scale-set-39 runner to its Firecracker
PID. cache-host authenticates the socket peer, then resolves the bound runner's
job UUID and request ID from host-held sources: the scale-set JobStarted record
for the runner ID, or before it arrives, the run's in-progress REST job on that
runner tied by display name to its one JobAssigned record. Guest claims alone
never authorize a grant. The per-VM vsock ports are:

- 1026: one setup per job; resolve and pin the job within the setup budget,
  verify run, attempt, repository, branch, SHA and host-held workflow ref,
  then sign an Ed25519 repository/ref grant.
- 1027: translate supported cache requests to cache-data over authenticated TLS.

Setup failures disable regional caching without failing the job, including an
unresolved or ambiguous job (a rerun before JobStarted repeats display names).
Clock, key, CA or token failure only disables setup. Grants expire within the
verified job deadline and 30 minutes; renewal rechecks the running job. Host
time comes from adjtimex, with uncertainty checked before granting or serving.
Signing keys and Garage credentials never enter the guest.

cache-data stages uploads privately, journals intent before PUT and publication
before success, and exposes only complete immutable archives. Downloads verify
size and SHA256 before serving bytes. Read tickets last at most one minute and
never outlive their grants. Quotas, physical staging and size-scaled operation
and recovery budgets remain finite. Active operations pin entries without
holding the index lock over storage I/O; hidden entries retry deletion with backoff.

The private journal uses flock, fsync and atomic replacement. Startup wipes
staging and verifies indexed objects: missing/corrupt objects become misses;
unreachable Garage refuses startup. Unknown objects are never adopted. Preserve
the journal, storage identity and historical prototype/ object prefix on upgrade.

See [deployment](../deploy/blr-garage/README.md),
[launcher](../deploy/standalone-guest/README.md),
[runner integration](../integrations/actions-runner/regional-cache.md) and
[validation](../docs/regional-cache-validation.md).

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

Managed storage uses signed optional `tenant_id` claims (positive 32-bit Central
tenant IDs); omission retains the original standalone grant encoding. Repository
scope keys are canonical JSON tuples: `["managed","<tenant>","<repository>"]`
versus the unchanged `["standalone","<region>","<repository>"]`. Ref priority,
restore keys, versions and per-repository quota semantics are unchanged.

The store's `BucketProvider` enumerates known tenants and supplies each tenant's
current bucket/credentials. Managed mode requires HTTPS with an explicitly
pinned PEM CA, normal hostname verification and finite transport/storage
deadlines; there is no skip-verification option. Standalone still accepts
literal-loopback HTTP. Credentials are absent from grants and journals.

A managed service configures a stable, exclusively owned
`managed/<service-id>/` prefix (service ID: letters, digits, hyphen, underscore).
Objects use `<prefix><tenant>/<entry-id>` within each team's bucket. On a fresh
journal, startup enumerates all known tenant buckets and deletes only that
service prefix before persisting an empty index or serving. Enumeration must be
complete; failures refuse startup and retry the reset next time. Other service
prefixes and standalone `prototype/` objects remain untouched. An existing
journal verifies only indexed objects and never adopts unknown objects.
The provider must honor deadlines and support concurrent credential lookups,
including rotation; the Central implementation is a separate issue. The CLI's
standalone Garage config accepts optional `ca_pem` for HTTPS; managed service
wiring supplies the provider through `store.Config`. Old standalone journals
and object keys require no migration. A store instance selects one mode and
refuses grants from the other mode.
