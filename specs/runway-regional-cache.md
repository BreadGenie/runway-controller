# Regional Actions cache

Status: draft design. No cache service, routing change or storage deployment is implemented.

## Purpose

Keep existing workflow files and supported cache actions unchanged while serving dependency-cache archives from storage near Runway runners. Start with the measured Yarn cache path used by `actions/setup-node`. The [performance plan](runway-performance-optimization.md) owns comparison conditions and acceptance targets.

The retained passing Server shard restored the same 584,388,223-byte archive in 623.766 seconds on BLR and 7.627 seconds hosted. Reported progress places most delay before the complete download. A separate host-only public download reached approximately 112 MiB/s, but used a different endpoint. These observations justify investigating regional caching; they do not establish the cause of the current GitHub cache slowdown or prove regional-cache performance.

## Compatibility contract

- Preserve supported upstream cache actions and setup actions without workflow edits. Record exact tested action and runner versions; do not claim compatibility with every action by inference.
- Preserve cache key, version, exact-hit, restore-key, lookup-only, save/restore and supported archive semantics. Verify current protocol behaviour from the pinned clients before implementing it.
- Preserve repository, branch, tag and pull-request access boundaries, including fork jobs. The control plane derives permissions from authenticated job context; a guest-supplied repository, key or ref is not authority.
- Keep artifacts, logs, job assignment, execution release and ordinary network traffic on their existing paths. Do not treat a shared service URL or token as cache-only without proving the protocol boundary.
- Cache failure remains visible and follows the supported action's contract. A cache miss must not become false success or silently broaden access.

## Storage and trust proposal

Use a regional archive service with immutable published entries and a fresh restore into each disposable guest. Shared writable guest mounts and sticky disks are outside this first feature. This is a proposal for design review, not approval of a new runtime trust boundary.

Uploads remain private and incomplete until the service validates their bounded size and commits the complete entry atomically. Concurrent writers cannot replace a committed entry. Aborted uploads have bounded lifetime and cleanup. Readers cannot see a partial archive. Caches remain untrusted job data and never become trusted runner image or bootstrap input.

Use narrowly scoped, expiring capabilities for cache operations, with explicit repository/ref access, method, object and size restrictions. Never expose service-wide or storage-account credentials to a guest. Digest-based deduplication must not expose cross-tenant existence or grant cross-tenant access.

Account for storage and transfer independently from executable guest capacity. Define per-repository and tenant quotas, upload/download concurrency, retention, eviction, deletion and operator observability before deployment. Eviction produces a cache miss; it must not alter an entry while readers use it.

## Implementation sequence

1. Trace the actual pinned `setup-node` and cache clients: discovery, authentication, lookup, segmented download, upload and publication. Identify a supported integration seam that preserves action integrity and does not redirect unrelated traffic. A hostname or environment override alone is not a proven solution.
2. Specify identity and access decisions with real branch/default-branch/PR/fork cases. Establish control-plane ownership and inspect existing artifact infrastructure before selecting a new service or storage system. Cargo is a candidate in the architecture, not a selected implementation.
3. Build a local protocol prototype with disposable storage and fixture credentials. Exercise the actual pinned action/client where possible. Demonstrate miss, save, hit, restore-key, concurrent save, interrupted upload, expiry and denied cross-repository/ref reads.
4. Measure pinned archive transfer and extraction separately against the existing path. Preserve the fixed guest shape and record cache state and failures. Diagnose any guest/network bottleneck independently of introducing the service.
5. Deploy only to declared development infrastructure after review of the trust and resource contract. Run the unchanged Server workflow and retain full cleanup evidence. Repeated matched qualification remains required before claiming parity.

## Open decisions

The protocol integration mechanism, service owner, storage backend, GitHub-cache migration/fallback policy, quota values and outage behaviour are unresolved. Do not choose them implicitly in implementation. In particular, never bypass cache authorization by globally proxying authenticated GitHub traffic, and do not replay uncertain writes across two backends without a defined identity and publication rule.

## Validation

Acceptance requires upstream-client compatibility, no cross-tenant or disallowed ref access, immutable atomic publication, bounded resource use and cleanup, correct behaviour under cancellation/concurrency/outage, and measured improvement with unchanged workflows. Keep all existing authority, clock, admission, network and execution deadlines intact. Any runtime change renews its affected qualification; no charge deletion or unqualified guest is permitted for benchmarking.

Blacksmith documents [transparent colocated Actions caching](https://docs.blacksmith.sh/blacksmith-caching/dependencies-actions) and a separate [sticky-disk snapshot mechanism](https://github.com/useblacksmith/stickydisk#architecture). These are design references, not evidence of our implementation or disclosure of their internal routing mechanism. GitHub's [dependency caching reference](https://docs.github.com/en/actions/reference/workflows-and-actions/dependency-caching) owns the upstream access and matching semantics to verify against the selected client revision.
