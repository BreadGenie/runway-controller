# Regional cache validation

The [spec](../specs/runway-regional-cache.md) defines the standalone contract.
Current review evidence belongs in
`.scratch/runway-regional-cache/review-shrink-20261105/`.

Required gates: Go build, vet and full tests; targeted cache race tests;
pinned Node clients through the relay; offline runner/cache Python suites;
initializer overlay tests. Real Garage smoke testing is opt-in.

Live proof must preserve the journal, demonstrate a warm hit, and run two
scale-set-39 shards with confirmed jobs, grants and passing Integration.
Record restore times and refuse forged run, sibling UUID, unbound guest and
tag-as-branch requests. Tear down all test guests and runners afterwards.
The hosted Migration failure due to MariaDB is unrelated to cache validation.

The network probe found the base guest path was not the bottleneck: public download reached 198–213 MB/s, near the warm host.
Probe scripts and raw measurements live in `.scratch/runway-regional-cache/net-bottleneck-20261104/`.

Repeated matched performance qualification and a standalone Firecracker jailer
remain follow-ups.
