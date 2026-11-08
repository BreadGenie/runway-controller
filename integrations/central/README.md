# Central integration patch

`central-runway-pilot.patch` adds Runner Pool and Runner Lease records and the authenticated controller RPCs to the inspected Central revision `6676d3178943d332c288fc55c04e6f44a8d01732`. It is a reviewable pilot integration, not an upstream release.

Apply it to a matching Central checkout, install that checkout in the intended bench, and migrate the Central site. Review `spec/RUNWAY.md` in the patched checkout for the contract and limits. Validate `central.tests.test_runway` on a separate test site before enabling a pool.

An operator creates a Runner Pool with the Team, region, available image, GitHub repository and dedicated scale-set ID, guest SSH public key, resources, concurrency, total lease deadline, and `controller_user`. Start with concurrency one. Use an API-key service user with `Central User` and a team-wide Developer or suitable custom role; do not grant System Manager. The service user must match the pool's assigned controller and have server view, create, and terminate capabilities.

Enable the Central scheduler and worker queues, with a ten-second bench scheduler tick. Its reaper continues VM expiry and tagged-orphan cleanup when the Runway process is unavailable. The Runway process must still reconcile GitHub registrations. A VM-deleted lease continues to consume capacity until GitHub cleanup is reported.

Central never stores the GitHub App key or JIT runner configuration. Keep the API credential and controller runtime files outside this repository. The controller uses Frappe's normal API-key authentication and fenced, pool-scoped RPCs.

A create request with an uncertain outcome is not retried. If no VM can be discovered by its durable attempt tags, the lease remains blocked for operator investigation. The pilot does not solve atomic Atlas create idempotency, commercial billing, or untrusted-job network isolation.

## Job evidence contract

The patch includes repository and request-label fields on Runner Lease and accepts them through `update_lease`. Nonempty job evidence is immutable; missing fields and zero request IDs preserve earlier observations. Direct assignment can reveal identity only after the runner exists. Job recording therefore does not enforce repository/image compatibility before VM creation, and must not gate demand-driven provisioning.

`go test ./...` exercises the controller and its HTTP adapter against the patch's actual route, lease transitions and snapshot using `contract_harness.py`. These tests require Python 3; they skip explicitly if it is absent. `python3 -m unittest discover -s scripts -p 'test_central_contract.py'` also checks the contract's refusal and replay rules. Only Frappe document storage, fencing and time are substituted; these checks do not establish database transaction or authorization correctness. Continue running the Central integration tests on a separate test site before enabling a pool.

For an existing pilot installation, update the Central code and migrate the new lease fields before running this controller. The patch targets the inspected base revision; do not apply it wholesale over an already-patched checkout.

JobAssigned evidence is attached only by an existing positive request ID or job UUID; the SDK message contains no runner name or ID. Ambiguous matches are refused and unmatched messages are ignored. Direct dispatch with request ID zero becomes attributable at JobStarted; a later JobAssigned can enrich that lease. No lease is selected merely because it is the only active lease.

The Central patch adds immutable `run_attempt`, `job_display_name`, `job_workflow_ref`, and `event_name` fields to `update_lease` and snapshots, plus a monotonic `started` snapshot flag set only by JobStarted (bootstrap alone does not establish a job start). The pinned scale-set SDK exposes no run attempt, so it remains unset; no attempt is inferred. Missing evidence preserves prior values. Migrate Central before deploying this controller. Replayed assigned/started evidence produces no update when nothing changes.

## Regional cache (issue 04)

Runner Pool `regional_cache` defaults off. The existing ten-second pool sweep
ensures one `<tenant>-<region>-runway-cache` Team Service per Team/region with an
Active flagged pool. Team-row locks and current pool reads serialize concurrent
reconciliation; two pools share a bucket. The hidden `Team Service.runway_cache`
ownership marker prevents adoption or removal of ordinary user buckets. Quota,
usage, billing and key rotation use the existing Team Service/storage API.

After the last flagged pool is disabled or deleted, reconciliation drains only
that marked cache bucket over S3, then uses Team Service's deletion hook. It also
scans marked services so deleting the last pool cannot strand its bucket.
Failures retain the record and keys for retry; Cargo's nonempty-bucket guard
remains in force. Lifecycle is eventual, normally one scheduler tick, outside
pool-save transactions to preserve the Team-before-Pool lease-admission lock
order. An uncertain Cargo create receipt may require operator recovery;
deterministic names prevent a second bucket or takeover.

Save an HTTPS `cache_service_url` on Region, then invoke the operator-only
`enroll_cache` document method. Repeat to rotate its credential. Central POSTs
`/configure` with `Authorization: Bearer <jwt>`. This dedicated five-minute
regional token has issuer `central`, audience `runway-cache:<numeric region id>`
and scope `runway-cache-configure`, verified with Central's issuer URL/JWKS
(`central.sso.central_url()` / `jwks_url()`). The cache service never accepts or
holds Cargo-audience tokens. The JSON body is exactly `{central_url, region, credential}`. Success is HTTP 200
with exactly `{"configured": true}`; redirects are refused. The cache service
must implement this contract; Cargo needs no change. A timeout or Central
rollback after remote acceptance requires re-enrolment.

The opaque read token permits only GET `central.runway.cache_lease` (address)
and `central.runway.cache_bucket` (team), with raw JSON and no `message` envelope.
A disabled, roleless service identity (`runway-cache@central.invalid`) exists
solely for this restricted auth hook; it receives no Central role or team.
Reads always check the credential's region. Job evidence preserves unknown
`run_attempt: 0`; `job` is null before assignment/start. Missing reads return 404,
ambiguous leases 409, with exactly an `error` key on these responses. See the
patched `spec/RUNWAY.md` for response keys and configure details.

Migrate Region, Runner Pool, Runner Lease and Team Service, then run both
`central.tests.test_runway` and `central.tests.test_runway_cache` on a separate
test site. Cache tests cover real database lifecycle and concurrent reconciles;
Cargo/S3 and the billing catalog are mocked. No regional hosts are contacted.

From the bench, the integration commands are:

```sh
bench --site central-runway-test.localhost run-tests --module central.tests.test_runway
bench --site central-runway-test.localhost run-tests --module central.tests.test_runway_cache
```

For an isolated source copy, put the patched checkout first in `PYTHONPATH` and
run the bench's Python with `-m frappe.utils.bench_helper frappe` from its `sites`
directory. Reload the test-site DocTypes from that copy first; this avoids
modifying the dirty pilot checkout. Use the vendored MariaDB 11.8.6 for the local
bench data directory. The cache test module also inherits the pilot lease tests.

Managed cache-data calls `cache_bucket?tenant_id=<signed tenant>` because grants
carry the tenant identity rather than the Central Team name. The regional read
credential may use either `team` or `tenant_id`, never both. The latter must map
to exactly one Team; missing, invalid or ambiguous mappings fail closed with
`{"error":"not_found"}`. Response keys are identical to the team lookup.

Q25: The controller verifies branch push/workflow_dispatch runs and the bound in-progress job using its App credential. It records immutable verified `ref`, `sha`, `event_name`, `run_attempt`, `job_workflow_ref`, `repository_id`, and `default_branch` on the lease. Failed verification records `verified: false` and a short `verify_reason`. The service requires `job.verified`, matching guest context and mesh tenant; renewal rechecks the same active lease and job in Central, without GitHub. The exact `cache_lease` job keys add `ref`, `sha`, `verified`; top-level keys add `repository_id`, `default_branch`.

Runner Pool `placement_rules` optionally holds a JSON array of Atlas placement rules. Central forwards nonempty rules on runner creation; an empty field preserves regional placement. Drain leases before changing it. Use an enforced Metal Server affinity for a pool that must stay on one development host.
