# Architecture

Runway should be a capability inside Frappe Cloud. A separate runner-controller process owns GitHub polling and runner lifecycle; Central owns the customer relationship and authorization. The current repository proves execution primitives and supplies operator tools for that design.

## Ownership

| Component | Responsibility |
| --- | --- |
| Central | Teams, GitHub installations, connected repositories, runner pools, capabilities, quotas, usage, and eventual billing |
| Runway controller | GitHub demand, admitted capacity, runner leases, JIT bootstrap, deadlines, recovery, and cleanup |
| Atlas and Metal | Regional placement, VM and disk lifecycle, images, guest networking, Firecracker execution |
| Cargo | Candidate image-building and regional artifact/cache infrastructure; not required by the current execution path |
| Pilot | Optional Frappe-specific guest tooling and later preview/deployment integration |
| GitHub Actions | Workflow scheduling and execution protocol, job results, checks, logs, artifacts, and existing cache behavior |

Central's planned CI records and the Runway controller are not implemented. The prototype created VMs through Central's existing Team-authorized Atlas client, then used separate operator tools for GitHub registration and listening. That did not create a Central CI lease, usage ledger, or customer VM billing record.

## Proposed execution path

```text
GitHub Actions <-> Runway controller -> Atlas -> Metal -> fresh runner VM
                         ^                              |
                         |                              +-> GitHub Actions
                      Central
                 identity and policy
```

An admitted runner lease should persist intent before work is acknowledged. Provisioning then converges asynchronously: create a fresh VM, deliver one-use bootstrap material, execute one job, export diagnostics, and confirm runner and VM removal.

The live prototype's journal binds a directly assigned job to the expected runner name, runner ID, and job UUID. GitHub supplied a zero request ID and result `succeeded`; the receipt preserves the raw result and normalizes success. The explicit job-acquisition path has isolated tests, but it has not been exercised successfully against GitHub.

## Boundaries that matter

Creation timeouts are uncertain outcomes. Persist an attempt identity before calling Atlas, reconcile by identity, and avoid blind creation retries. A tag helps locate resources but is not an atomic idempotency guarantee.

VM deletion is asynchronous. An accepted DELETE is not completion; confirm absence through the tenant-scoped regional API. The prototype verified automatic Atlas cleanup after fixing scheduler configuration. A production controller still needs independent orphan sweeps and deadline handling.

Keep GitHub App keys and control-plane credentials outside guests. A guest receives only its JIT runner configuration or a narrowly scoped bootstrap credential. Docker runs inside the VM; no host Docker socket is shared. The current host setup does not prove untrusted-job egress isolation or denial of access to other infrastructure.

Repository connections and commit identities should support future previews and deployments. Their lifecycles remain separate: a runner ends with its job, a preview remains until its branch or PR closes, and deployment access requires explicit authorization.

## Upstream contracts

- [Atlas architecture at the inspected revision](https://github.com/frappe/atlas/blob/339735de16471e90232cf8b1ff840696ee830ad9/docs/start/architecture.md)
- [Central coordination with Atlas and Pilot](https://github.com/frappe/central/blob/6676d3178943d332c288fc55c04e6f44a8d01732/spec/ATLAS_COORDINATION.md)
- [GitHub scale-set listener contract](https://github.com/actions/scaleset/blob/e6daac702355cdb5b880b4fbdcf6d85dcd9e48e5/listener/listener.go)

These references describe the inspected revisions. Atlas and Central were pre-production during the experiment; verify contracts when updating dependencies.
