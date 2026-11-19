# Architecture

Runway should be a capability inside Frappe Cloud. A separate runner-controller process owns GitHub polling and runner lifecycle; Central owns the customer relationship and authorization. The current internal pilot implements Central-backed leases and a bounded controller, alongside the original execution tools.

## Ownership

| Component | Responsibility |
| --- | --- |
| Central | Current: Teams, runner pools and leases, capabilities, admission and regional cleanup. Planned: GitHub installations, repository connections, usage and billing |
| Runway controller | GitHub demand and events, Central lease transitions, JIT bootstrap, guest observation, and GitHub cleanup |
| Atlas and Metal | Regional placement, VM and disk lifecycle, images, guest networking, Firecracker execution |
| Cargo | Candidate image-building and regional artifact/cache infrastructure; not required by the current execution path |
| Pilot | Optional Frappe-specific guest tooling and later preview/deployment integration |
| GitHub Actions | Workflow scheduling and execution protocol, job results, checks, logs, artifacts, and existing cache behavior |

The [Runway Frappe app](../integrations/central/README.md) implements Runner Pool and Runner Lease records, pool-scoped authorization, ownership fencing, admission, regional reconciliation and expiry. Runway calls Central's authenticated API; Central creates VMs through its existing Team-authorized Atlas client. This is an operator-configured internal pilot, with no usage ledger or customer VM billing record.

## Pilot execution path

```text
GitHub Actions <-> Runway controller -> Central -> Atlas -> Metal -> fresh VM
                         |                |                          |
                         |           durable leases                  +-> GitHub Actions
                         +---- administrative guest transport ------>+
```

An admitted runner lease persists intent before work is acknowledged. Provisioning converges asynchronously: create a fresh VM from a baked image, deliver one-use bootstrap material, execute one job, export best-effort diagnostics, and confirm runner and VM removal. Central counts capacity until both VM and GitHub cleanup finish. The controller currently accepts concurrency one; its administrative transport targets one configured Metal host.

The live prototype's journal binds a directly assigned job to the expected runner name, runner ID, and job UUID. GitHub supplied a zero request ID and result `succeeded`; the receipt preserves the raw result and normalizes success. The durable controller also persists job-acquisition intent. Its restart and uncertain-outcome behavior has isolated tests; those tests are separate from live controller recovery evidence.

The controller lifecycle module keeps one lease engine across GitHub message sessions. It owns connection retries, ownership renewal, capacity refresh, and shutdown. Message handling and reconciliation run serially; the SDK receives a successful delivery result only after handling succeeds. A disconnected listener retains its error cause and retries after a bounded delay. While GitHub cannot connect, lease reconciliation continues. Ownership heartbeat failure cancels polling and prevents session-close mutations; normal shutdown waits for polling and renewal to end before returning.

The [guest agent](../guest/guest_agent.py) owns preparation, one-use bootstrap,
status, diagnostics collection, and stop behind `execute(request)`. It returns
an exit status and a public status document; its CLI reads the request from stdin.
The SSH transport sends this standalone module unchanged and carries its response.
Filesystem paths have fixed production defaults and an internal test adapter;
requests cannot override them. Tests call the same entry point with private files
and substituted system commands, without rewriting the program's source. Timing
and Docker rollback state belong to one request.

## Boundaries that matter

Creation timeouts are uncertain outcomes. Persist an attempt identity before calling Atlas, reconcile by identity, and avoid blind creation retries. A tag helps locate resources but is not an atomic idempotency guarantee.

VM deletion is asynchronous. An accepted DELETE is not completion; confirm absence through the tenant-scoped regional API. The prototype verified automatic Atlas cleanup after fixing scheduler configuration. The Runway app runs independent orphan sweeps and deadline handling. Live failure drills are still required to validate these paths in operation.

Keep GitHub App keys and control-plane credentials outside guests. A guest receives only its JIT runner configuration or a narrowly scoped bootstrap credential. Docker runs inside the VM; no host Docker socket is shared. The current host setup does not prove untrusted-job egress isolation or denial of access to other infrastructure.

Repository connections and commit identities should support future previews and deployments. Their lifecycles remain separate: a runner ends with its job, a preview remains until its branch or PR closes, and deployment access requires explicit authorization.

See [pilot operations](pilot.md) for installation, fencing, recovery, and current deployment limits. Future GitHub installations and repository connections still require a customer-facing Central flow.

## Upstream contracts

- [Atlas architecture at the inspected revision](https://github.com/frappe/atlas/blob/339735de16471e90232cf8b1ff840696ee830ad9/docs/start/architecture.md)
- [Central coordination with Atlas and Pilot](https://github.com/frappe/central/blob/6676d3178943d332c288fc55c04e6f44a8d01732/spec/ATLAS_COORDINATION.md)
- [GitHub scale-set listener contract](https://github.com/actions/scaleset/blob/e6daac702355cdb5b880b4fbdcf6d85dcd9e48e5/listener/listener.go)

These references describe the inspected revisions. Atlas and Central were pre-production during the experiment; verify contracts when updating dependencies.
