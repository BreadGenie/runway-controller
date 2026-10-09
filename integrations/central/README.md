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
