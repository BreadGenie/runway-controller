# Roadmap

The product direction is Frappe Cloud integration with internal dogfood first. The execution experiment is complete and a bounded durable pilot is implemented. One live restart and independent deadline cleanup passed; broader recovery evidence and customer readiness remain separate milestones.

## Milestones

| Milestone | Work | Acceptance evidence |
| --- | --- | --- |
| Execution prototype | Fresh VM, official runner, Docker service, durable message receipt, cleanup | Completed for two successful workflows; scope in [results](poc-results.md) |
| Durable runner controller | Implemented: Central-backed leases, fenced admission, uncertain-create recovery, deadlines, cancellation, orphan cleanup, baked image | Automated tests, same-owner live restart, and independent VM deadline cleanup pass; broader failure drills remain |
| Central dogfood product | Shared GitHub connection, repository access, pool configuration, diagnostics, usage records | One internal team can operate a non-required workflow without database edits |
| Compatibility and performance | Representative unit, Docker, and Frappe integration jobs; image maintenance | Measured queue-to-start and workflow times, clear toolchain differences, repeatable image builds |
| Caching | Evaluate nearby dependency storage, Docker build cache, and image distribution | Measured hit rates and time saved; verified tenant and trust boundaries |
| Customer readiness | Isolation, fork-PR admission, quotas, billing semantics, host draining, failure recovery | Operational and isolation evidence sufficient for the intended customer scope |
| PR previews | Create and update a preview for a PR; remove it on close | Commit-linked preview lifecycle with explicit data and secret policies |
| Deploy on push | Deploy a tested revision after required checks; later add promotion and approvals | Exact commit/artifact identity, controlled credentials, deployment history and recovery |

The pilot has no shared cache. The baked image removes package installation from normal guest startup, but does not retain job state between runs. Start with GitHub's existing artifact and cache behavior. Cache design should follow workload measurements; cross-job writable state introduces a separate trust problem.

See [pilot operations](pilot.md) for the implemented controller and its current limits. See [recorded live evidence](poc-results.md) for the tested restart and cleanup boundaries.

## First rollout

Select one Frappe-owned repository and a trusted, manually dispatched or non-required workflow. Keep the existing required checks while measuring reliability. The execution prototype used a private scratch repository; it has not migrated an existing Frappe CI workflow.

Progress to Docker and Frappe integration jobs before moving required checks. Public repository ownership does not make fork PR code trusted. Admit those jobs only after defining and validating their isolation policy.

## Decisions for the team

- Where should the controller run in an operated FC region, and how should the existing pool/lease records connect to usage and billing?
- What authoritative recovery path should release an uncertain Atlas create that cannot be discovered? The pilot safely retains its capacity; atomic create idempotency could improve availability.
- Which repository and workflow provide the first representative dogfood baseline?
- Which network and secret boundaries are required for the initial trusted scope and later fork PRs?
- What image compatibility, concurrency, and cost targets should be measured before caching or commercial rollout?

Performance and savings are open questions. The prototype establishes feasibility, not a production latency or cost advantage.
