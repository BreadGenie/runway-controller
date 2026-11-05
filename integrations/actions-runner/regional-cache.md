# Runner regional cache integration

See the [standalone spec](../../specs/runway-regional-cache.md).
`regional-cache.patch` applies to the maintained Actions runner after its
administrative gate patch. It freezes job context before authorization and runs
regional setup before any steps. Setup exceptions disable regional caching;
job cancellation still propagates. Container jobs retain upstream routing.

Supported pinned revisions:

- actions/cache@0057852bfaa89a56745cba8c7296529d2fc39830 (v4.3.0)
- actions/setup-node@249970729cb0ef3589644e2896645e5dc5ba9c38 (v6.5.0)

Routing uses resolved action identity, never a mutable action tag alone.
Only the cache-v1 path is replaced; artifact inputs and runtime tokens remain
on their existing paths. The same immutable routing choice covers pre/main/post.
Unsupported revisions, events and contexts retain GitHub caching.

## Guest helpers

cache-setup exchanges a bounded JSON request with the host over vsock 1026.
The request includes job UUID, request ID, nonce and frozen GitHub context.
An enabled response selects the relay at 127.0.0.1:18080; no storage or signing
credentials enter the guest. Invalid, missing and timed-out responses disable
setup. The helper never retries an uncertain setup.

cache-relay forwards bounded connections to vsock 1027. Its 330-second idle
limit covers the host request deadline, including large-save finalization.
The standalone launcher copies both helpers and the relay systemd unit into
each guest clone so branch changes reproduce the guest integration.

## Runner build

The deployed runner must be rebuilt when the C# patch changes. Updating the
Python helpers alone cannot update the runner's exception handling or container
routing. Use an unregistered build distribution; never copy runner credentials,
workspaces or diagnostics into the image. The repository's initializer overlay
preparation and image sealing scripts enforce that input boundary.

## Validation

Offline Python tests use real sockets for setup and relay behavior.
The Go pinned-client test runs both upstream bundles through the relay and TLS
transport, checks exact bytes, restore keys and multiple upload chunks, and
rejects client connections outside the relay.
Runner L0 coverage checks setup exceptions, cancellation, container routing and
resolved action identity. Live proof requires two confirmed jobs and warm hits.
