# Runway

Runway provides disposable GitHub Actions runners backed by Frappe Cloud capacity.

## Language

**Runner Pool**:
A repository's configured source of runner capacity, with an image, machine shape and concurrency limit.

**Runner Lease**:
A durable record of one runner attempt, from capacity reservation through confirmed runner and VM cleanup.

**Job evidence**:
Observed facts tying a GitHub Actions job to a Runner Lease, including its job identity, repository and requested labels. Evidence may arrive before dispatch or after the runner starts the job.

**Cache session**:
One job's temporary access to the regional cache, tied to its runner identity and lifetime. Standalone and managed runners each have their own cache session.
