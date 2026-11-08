# Managed regional cache integration

Run from the repository root. Tests are skipped unless explicitly enabled; no
network downloads, live mesh peers, Central installation, or host credentials
are used. The suite binds the guest relay's fixed `127.0.0.1:18080`, so do not
run multiple pinned-client suites concurrently.

```sh
export PATH=/home/t3code/.local/share/fc-ci/go/bin:$PATH GOTOOLCHAIN=local
export NODE_BIN=/home/t3code/.local/opt/node/bin/node
export RUNWAY_CACHE_VENDOR=/home/t3code/projects/exp/runway/.scratch/runway-regional-cache/vendor
RUNWAY_MANAGED_E2E=1 go test -race -run TestManagedCacheEndToEnd -v ./...
```

The default integration backend is explicitly an **in-memory S3 fake**. To use
two disposable, hash-verified Garage v2.3.0 instances shared across pinned
client cases, install the existing Garage harness's Python boto3 prerequisite and
set `RUNWAY_GARAGE_TEST=1` on the command above. Each instance owns one team's
bucket and key. A loopback TLS gateway fronts Garage's HTTP S3 listener; all
managed-adapter requests therefore use TLS. Garage is terminated and reaped on
cleanup. Archives contain 2 MiB of random data, exercising multiple PATCHes.

A fast CI variant needs only Go and no Node, Python, bundles, or Garage:

```sh
RUNWAY_MANAGED_E2E=1 RUNWAY_MANAGED_E2E_OFFLINE=1 \
  go test -race -run TestManagedCacheEndToEnd -v ./...
```

The full in-memory variant also works offline after provisioning the pinned
vendor directory, Node, Yarn, tar and zstd. Bundles are verified against the
cache-node allowlist before execution. Six actions/cache cases (v4.3.0, v5.1.0,
v6.1.0, both main/post and restore/save-only) and two setup-node cases (v6.5.0,
v7.1.0) run.

## Boundaries and assertions

The setup/translator, configure listener and managed data store are real Go
handlers on separate loopback TLS listeners. Configure enrolment uses a real
EdDSA JWT and fake Central JWKS. Central's HTTPS endpoints enforce regional
credentials and return the exact response objects, including raw error JSON.
The bucket API supports the tenant lookup added in the companion fix: cache-data
has a signed tenant ID, whereas setup has a Central Team name.

A TLS listener stands for a mesh peer. A test-only outer handler assigns
`Request.RemoteAddr` to `fdaa:1:0:<tenant>::<vm>` before entering the unmodified
service attribution code. Guests cannot select an address through a header.
This verifies attribution and tenant checks, **not wg-mesh source filtering**.
Fake GitHub REST uses recorded responses served over loopback TLS. The real
controller assignment resolver persists evidence before JobStarted; that
persisted evidence is what fake Central publishes to setup.

The cache-node subprocess runs its production allowlist, per-job route decision,
service setup exchange and environment routing. Test-only injection supplies a
loopback service name, CA roots and temporary state paths so CI does not need
root-owned `/etc/runway` or `/run/runway`. The process execs the real Node binary
with the pinned action; action paths have the runner's `_actions/actions/` shape.
The relay uses its production TLS connector. A Node network guard denies direct
service connections; permitted connections must reach the fixed guest relay.

| Scenario | Assertion |
| --- | --- |
| Miss/save/exact/restore-key | Every pinned cache case restores identical file hashes; setup-node does miss/save/exact with identical hashes. |
| Cross-team, same repository | Team B misses A's key and cannot redeem A's archive ticket. |
| Spoofed/unknown attribution | Invalid source region, unknown address and Central/source tenant mismatch produce no enabled setup. |
| Early setup | Controller REST resolver records assigned evidence, `started=false`; service independently verifies GitHub REST. |
| Unenrolled service | Setup returns disabled. |
| Stopped service | A fresh cache-node decision retains the fake GitHub cache URL; pinned restore exits zero and reaches that endpoint. |
| In-place data restart | The journal service ID is unchanged and lookup remains a hit. |
| Recreation | Removing the journal produces a different service ID and misses. |
| Lazy cleanup | Bucket use retains fresh stale prefixes, then a later pass removes aged stale objects. |
| Configure rotation | Second enrolment replaces the credential; fresh peer setup and invalidated bucket metadata use the new credential. |
| Guest custody | Parent audits guest files, route state, action environment and subprocess/relay logs for server credentials and signing-key encodings. |

Cleanup assertions always use the metadata-aware in-memory fake. The production
store forbids cleanup thresholds at or below two hours. The test uses a three-hour
threshold and ages only fake S3 metadata across it, avoiding hours of waiting
without weakening that safety check.

## Product fixes found during integration

- `c1961f6`: Central's patch accepted only `team`, but managed cache-data queries
  `tenant_id`. Add a mutually exclusive tenant lookup, rejecting invalid and
  ambiguous mappings. The raw response contract is unchanged. This extends the
  spec's request contract and requires the updated Central patch at deployment.
- `48e67a5`: accept the legacy escaped restore-key list delimiter used by the
  actions/cache v4 URL builder; regression covers standard and double escaping.
- `027b1a0`: preserve literal `%2C` cache keys when handling the legacy
  delimiter, with an exact-key regression.
- `3be3fe6`: refresh cache-data's regional read credential when bucket metadata
  refreshes, and attach the configured CA roots to its Central HTTPS transport.
  Enrolment previously updated only the setup process's credential snapshot.
- `63db561`: bind managed download tickets to the translator session that issued
  them. A different team's session could previously redeem a known archive URL
  despite lookup isolation. The standalone capability behavior is unchanged.

Still requiring a live environment: apply/run the Central patch in Frappe,
Atlas deployment and release installation, controller guest preparation and
root-owned configuration, internal DNS and real certificate provisioning,
wg-mesh source filtering, actual VM upgrade/migration/recreation and disk
custody, remote Cargo Garage quotas/billing/rotation, and a passing Server job
on a controller-provisioned VM. This suite does not qualify those boundaries.

## Verification

The default `go vet ./...` and `go test -race ./...` checks pass. The opt-in
lifecycle variant and all eight pinned-client cases pass with disposable Garage,
including stopped-service fallback. The lifecycle fake asserts tenant/ticket
isolation, journal recovery/recreation, rotation and cleanup timing. No push or
live deployment is part of this change.
