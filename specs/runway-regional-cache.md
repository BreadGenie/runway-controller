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
including rotation. The Central provider and process configuration are described below. The CLI's
standalone Garage config accepts optional `ca_pem` for HTTPS; managed service
wiring supplies the provider through `store.Config`. Old standalone journals
and object keys require no migration. A store instance selects one mode and
refuses grants from the other mode.

## Service mode

`runway --action cache-service --config /etc/runway/cache-service.json` serves
one direct mesh TLS listener. `POST /setup` carries the existing newline-framed
setup wire v2 request and response; cache v1 `/_apis/artifactcache/...` and
`/download/...` retain their paths on the same listener. The future guest relay
must map its setup exchange to `/setup` and proxy translator paths unchanged.
The response still names the fixed local relay, never a credential.

Configuration is a version-1 JSON object with exactly these required keys:

```json
{
  "version": 1,
  "region_id": "blr-1",
  "mesh_region_id": 1,
  "listen": "[fdaa:1::10]:8443",
  "audience": "https://cache-data.example.internal:8444",
  "certificate_file": "/etc/runway/service.crt",
  "private_key_file": "/etc/runway/service.key",
  "signing_key_file": "/var/lib/runway-cache/signing.key",
  "cache_data_ca_file": "/etc/runway/cache-data-ca.crt",
  "github_token_file": "/etc/runway/github-token",
  "central_url": "https://central.example.com",
  "central_token_file": "/etc/runway/central-region-token",
  "archive_bytes": 1073741824
}
```

`mesh_region_id` is the numeric 16-bit mesh region; `region_id` is Central's
region name and the grant region. `listen` must be a literal mesh IPv6 address
in that numeric region with a nonzero port. All file paths are absolute and
clean. Config, keys and tokens are root-owned private single-link regular files;
certificates and CA files may be readable but cannot be group/world writable.
The signing key is generated locally on first start in an existing root-owned
private directory and atomically published without replacing an existing key.
Only its public key belongs in cache-data's trusted issuers; no private key or
Central token goes to a guest. Central HTTPS uses system trust roots and token
authentication; redirects are refused. TLS ingress requires TLS 1.3.

Caller identity comes exclusively from the direct connection's IPv6 peer,
never forwarding headers. The layout is `fdaa | region (16) | tenant (32) |
padding (48) | VM (16)`; other prefixes, regions, IPv4 and tenant zero are
refused. Central must return that exact address, tenant and configured region,
an unexpired lease and job evidence. Missing, ambiguous, malformed or failed
lookups disable setup. GitHub REST then confirms the runner's in-progress job,
display name, run and attempt, followed by the existing repository, event,
branch, SHA, same-repository head and workflow-ref checks. Renewal rechecks
Central identity and GitHub job liveness. One enabled setup and nonce replay
fences remain per lease until its deadline; requests and live session counts
are bounded. A service restart clears in-memory setup fences, as standalone
host restart does. Setup work shares the existing 2.5-second budget.

## Regional process layout (managed storage)

Run two systemd units on the regional service VM. This reuses the existing TLS
boundary and gives cache-data sole custody of its journal and staging disk.
The service signs grants with the positive tenant from the verified Central
lease; managed cache-data rejects standalone grants. No changes to main's two
actions are needed.

- `runway-cache-data.service`: `ExecStart=/usr/local/bin/runway --action cache-data --config /etc/runway/cache-data.json`
- `runway-cache-service.service`: `After=runway-cache-data.service`,
  `Requires=runway-cache-data.service`,
  `ExecStart=/usr/local/bin/runway --action cache-service --config /etc/runway/cache-service.json`

Install the unit templates from `deploy/regional-cache/systemd/`.
Use `Restart=on-failure` for both units. Provision protected configuration and
directories before starting either unit. Generate the service signing key
locally (the service does this on first start), provision its public key in
cache-data, then start both. Keep the signing key and journal across upgrades.
Use a cache-data certificate whose SAN matches the service's audience, with
that CA in `cache_data_ca_file`. Bind cache-data to loopback; resolve its
audience hostname locally to loopback. The mesh listener stays on cache-service.

Managed cache-data uses the existing version-1 config, bounds, TLS files,
public keys and disk settings, but **replaces** the required `garage` object
with this required `managed` object (the two cannot coexist):

```json
{
  "managed": {
    "central_url": "https://central.example.com",
    "central_token_file": "/etc/runway/central-region-token",
    "storage_ca_file": "/etc/runway/garage-ca.crt",
    "prefix": "managed/blr-cache/",
    "teams": {"2": "internal-team", "3": "another-team"}
  }
}
```

The example above is the replacement fragment, not a complete configuration.
`teams` is a nonempty, durable, authoritative tenant-to-Central-team registry
for this service prefix. Deployment must enroll each team here **before**
enabling its pool and retain retired teams while their bucket may contain this
prefix. Back up this configuration independently of the journal and restore it
on VM recreation. Never rebuild it from active leases or currently enabled
pools: those omit idle and retired tenants. Never prune it merely because
Central returns 404. A deleted bucket requires explicit operator reconciliation
of the registry after verifying its objects are gone. Central's current contract
has no enumeration method; adding one and automating this registry is future
Central provisioning work, not an invented read API.

At cold start the provider enumerates the complete registry and freshly resolves
**every** team through `cache_bucket`. The store also validates every resolved
storage configuration before deleting anything. A missing team, malformed
response, bad credentials or enumeration error refuses startup and writes no
new journal. A storage error during deletion may leave a partly cleared
*unindexed* prefix; serving still never starts, and the next startup retries.
Existing journals never run the cold reset, so reset cannot delete objects
that a retained journal references. Reset deletes only this service prefix
and preserves other prefixes in every bucket.

Central responses use the same bounded HTTPS token client and exact JSON
contract as leases; redirects are refused. Tenant identity must match the
registry. Each tenant's endpoint, region, bucket and credentials are cached for
at most 30 seconds, with no stale-on-error behavior. Storage 401/403 or S3
authentication errors invalidate that tenant's credentials, causing a re-read
on the next operation. Failed writes are not automatically replayed. Explicit
rotation can also be picked up by restarting cache-data, or within the TTL.
Storage uses the locally pinned CA file, never ambient roots. Credentials stay
in process memory and are absent from journals, grants and logs.

Setup and renewal freshly check Central bucket availability as well as lease
authority. Data API authorization checks bucket availability (using the bounded
credential cache); errors refuse requests. Downloads still require their
short-lived journaled ticket and a successful tenant-routed storage read.
Central provisioning, Atlas deployment and the guest TLS relay remain separate
issues; no live regional deployment is qualified here.
