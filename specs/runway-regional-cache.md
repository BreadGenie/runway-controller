# Regional Actions cache

Status: standalone development deployment on blr-1.

## Scope

Serve dependency-cache archives near disposable Firecracker runners without
workflow changes. Supported pinned clients are actions/cache@0057852b (v4.3.0)
and actions/setup-node@24997072 (v6.5.0), using cache protocol v1.
Artifacts, logs and other action traffic retain their existing routes.
PRs, forks, tags and unsupported actions retain GitHub caching.

## Authority

The trusted root launcher registers an ephemeral runner in scale set 39 and
writes a root-only binding of VM, Firecracker PID, repository and runner identity.
cache-host checks the connecting socket peer against that PID.
The guest supplies claims, never authority.

cache-assignments is an admission dependency because its scale-set session acquires jobs for the scale set.
It records JobAssigned and JobStarted evidence in root-only files, pruned by age.
At port 1025 the host confirms the job UUID and request ID against that session
and REST, with a 35-second confirmation budget. The bound guest is released even
if confirmation fails; regional setup then stays disabled.
At port 1026 setup checks the confirmed job, exact run and attempt, branch, SHA,
same-repository head and the host-held workflow ref suffix. Only branch pushes
and manual dispatch qualify. One release permits one setup.

The host signs short-lived grants and renews them in the background after a
fresh running-job check. Stale session records require REST confirmation.
Clock uncertainty comes exclusively from adjtimex: unsynchronized clocks disable
setup. Clock, key, CA or token failure must never remove the 1025 listener.
cacheJobAuthority is the small seam for a future Central source; that integration
is outside this standalone implementation.

## Data path

cache-setup establishes the route once before steps. The runner patch isolates
setup faults from job execution and skips container jobs. A bounded relay carries
only supported cache requests to port 1027. cache-host injects the grant into a
TLS connection to cache-data. Guests never receive signing or Garage keys.

cache-data uses disk staging and a persistent single-node Garage on blr-1.
Published entries are immutable. Uploads reserve quota and remain invisible
until complete size and digest verification and durable publication.
Downloads hash while copying to a private spool and verify before serving bytes.
The journal retains the legacy prototype/ object prefix, storage identity,
publication intents and hashed read tickets. Startup verifies indexed objects
within a shared recovery budget. Missing or damaged objects become misses;
unreachable storage refuses startup. Unknown objects are never adopted.

Published uploads release their spool. In-use entries are refcounted; Garage I/O
and streaming run outside the global lock. A bounded background sweep retries
hidden-object cleanup with backoff. Disk custody uses flock and startup wiping.
Storage deadlines and reservation lifetime scale with size; ticket expiry is
checked before fetching. Cache loss never changes runner image contents.

## Validation and operations

Run the Go build, vet, unit and race gates, pinned clients through the relay,
and offline Python suites after each stage. Exercise real Garage separately.
Live validation requires two confirmed shards, grants, warm restores, passing
Integration and refusals for forged runs, sibling UUIDs, unbound guests and
tag-as-branch claims. Preserve the existing Garage journal across upgrades.
See [deployment](../deploy/blr-garage/README.md),
[launcher](../deploy/standalone-guest/README.md) and
[validation](../docs/regional-cache-validation.md).

Repeated matched performance qualification remains outstanding.
A Firecracker jailer for the standalone launcher is a follow-up.
