# Central integration patch

`central-runway-pilot.patch` adds Runner Pool and Runner Lease records and the authenticated controller RPCs to the inspected Central revision `6676d3178943d332c288fc55c04e6f44a8d01732`. It is a reviewable pilot integration, not an upstream release.

Apply it to a matching Central checkout, install that checkout in the intended bench, and migrate the Central site. Review `spec/RUNWAY.md` in the patched checkout for the contract and limits. Validate `central.tests.test_runway` on a separate test site before enabling a pool.

An operator creates a Runner Pool with the Team, region, available image, GitHub repository and dedicated scale-set ID, guest SSH public key, resources, concurrency, total lease deadline, and `controller_user`. Start with concurrency one. Use an API-key service user with `Central User` and a team-wide Developer or suitable custom role; do not grant System Manager. The service user must match the pool's assigned controller and have server view, create, and terminate capabilities.

Enable the Central scheduler and worker queues, with a ten-second bench scheduler tick. Its reaper continues VM expiry and tagged-orphan cleanup when the Runway process is unavailable. The Runway process must still reconcile GitHub registrations. A VM-deleted lease continues to consume capacity until GitHub cleanup is reported.

Central never stores the GitHub App key or JIT runner configuration. Keep the API credential and controller runtime files outside this repository. The controller uses Frappe's normal API-key authentication and fenced, pool-scoped RPCs.

A create request with an uncertain outcome is not retried. If no VM can be discovered by its durable attempt tags, the lease remains blocked for operator investigation. The pilot does not solve atomic Atlas create idempotency, commercial billing, or untrusted-job network isolation.
