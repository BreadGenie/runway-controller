# Standalone cache data service

`runway --action cache-data --config /etc/runway-cache/cache-data.json`
serves TLS on 127.0.0.1:18445. See the [spec](../../specs/runway-regional-cache.md).
The root-owned 0600 configuration contains version, region_id, audience,
public_origin, certificate_file, private_key_file, state_dir,
public_keys, garage, disk, storage_budget and quota limits.
The clock always uses host_ntp via adjtimex; no clock_source key is accepted.
Use `garage`, not the retired garage_fixture key.

Garage contains endpoint, region, bucket, access_key and secret_key.
Only an explicit loopback HTTP endpoint is accepted. The bucket key remains on
the host; guests receive neither storage credentials nor grant signing keys.
The TLS audience and public origin must agree. Forwarded headers are ignored.

## Bounds

Archives are capped at 1 GiB; the deployed logical cache is 4 GiB.
Disk staging has an independent physical budget, with custody under flock.
Startup wipes the owned staging directory. Publication releases upload staging.
Requests use signed repository/ref grants. Published entries are immutable.
Concurrent reservations have one winner; incomplete archives cannot be read.
Read tickets are one-minute capabilities, capped by their grant and journaled
as hashes. Expiry is checked before storage access, not during a long fetch.
Downloads copy and hash once, verify the full size and digest, then serve bytes.

Storage operations use base_seconds + bytes/min_bytes_per_second, capped at
max_seconds. Defaults are 12 seconds, 16 MiB/s and 300 seconds.
Recovery shares a budget including one second per object, capped by
recovery_max_seconds (default 900). Reservation lifetime includes the size budget.
Slow readers and storage operations do not hold the global metadata lock.
Hidden entries remain charged until a bounded background sweep deletes them;
failures retry with backoff. Active readers prevent deletion.

## Recovery

The private journal persists object intents before PUT and publication before
success. Atomic replacement uses file fsync, rename and directory fsync.
Journal failure fences storage effects until restart. Recovery verifies each
indexed published object. Missing or corrupt data becomes a miss; unavailable
Garage fails startup so systemd can retry. Unfinished uploads become cleanup
intents. Ticket hashes and original expiries survive restart.
The historical prototype/ prefix remains for compatibility with the live journal.
Unknown Garage objects are never adopted or scanned.

## Deployment

[install.sh](../../deploy/blr-garage/install.sh) preserves previous binaries,
installs hardened units and restarts binary consumers. Configuration migration
must rename garage_fixture to garage before restart, preserving all other values.
Keep the journal and Garage directories. Do not regenerate the host GitHub token.
Health checks cover service state, private listeners, TLS, clock and disk space.
The launcher injects current relay/setup helpers into each disposable clone.
Rebuild the image's runner when changing the C# patch.
