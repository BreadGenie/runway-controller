# Regional Actions cache

Standalone deployment on blr-1 for actions/cache@0057852b (v4.3.0) and
setup-node@24997072 (v6.5.0), cache protocol v1. Other traffic keeps its route.
Only same-repository branch pushes and workflow_dispatch qualify; PRs, forks,
tags, container jobs and unsupported revisions retain GitHub caching.
A failure before setup also keeps GitHub caching. After setup, a failed
restore or save is a cache miss or warning, not a retry against GitHub;
fail-on-cache-miss still fails the job as it would upstream.

The root launcher binds each ephemeral scale-set-39 runner to its Firecracker
PID. cache-host authenticates the socket peer, then confirms the job UUID and
request ID against the scale-set session and GitHub REST. Guest claims alone
never authorize a grant. The per-VM vsock ports are:

- 1025: confirm within 35 seconds and release the bound job even on failure.
- 1026: one setup per release; verify run, attempt, repository, branch, SHA and
  host-held workflow ref before signing an Ed25519 repository/ref grant.
- 1027: translate supported cache requests to cache-data over authenticated TLS.

Setup failures disable regional caching without failing the job. Clock, key,
CA or token failure must leave the gate available. Grants expire within the
verified job deadline and 30 minutes; renewal rechecks the running job. Host
time comes from adjtimex, with uncertainty checked before granting or serving.
Signing keys and Garage credentials never enter the guest.

cache-data stages uploads privately, journals intent before PUT and publication
before success, and exposes only complete immutable archives. Downloads verify
size and SHA256 before serving bytes. Read tickets last at most one minute and
never outlive their grants. Quotas, physical staging and size-scaled operation
and recovery budgets remain finite. Active operations pin entries without
holding the index lock over storage I/O; hidden entries retry deletion with backoff.

The private journal uses flock, fsync and atomic replacement. Startup wipes
staging and verifies indexed objects: missing/corrupt objects become misses;
unreachable Garage refuses startup. Unknown objects are never adopted. Preserve
the journal, storage identity and historical prototype/ object prefix on upgrade.

See [deployment](../deploy/blr-garage/README.md),
[launcher](../deploy/standalone-guest/README.md),
[runner integration](../integrations/actions-runner/regional-cache.md) and
[validation](../docs/regional-cache-validation.md).
