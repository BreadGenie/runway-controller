# Runway Frappe app

Install the standalone `runway` app on a site running Central. It owns the
Runner Pool and Runner Lease DocTypes and serves the controller's POST methods
under `runway.api.*`. See the [pilot setup](../../docs/pilot.md).

Use a dedicated API-key user with `Central User` and Team capabilities
`server:view`, `server:create` and `server:terminate`. Assign it to the pool.
Keep credential files outside this repository. Enable the site's scheduler and
worker queues for lease expiry and orphan cleanup.

Runner Pool `placement_rules` accepts an optional JSON array of Atlas placement
rules. Use enforced Metal Server affinity for a pool pinned to one dev host.
An uncertain create is reconciled by lease tags without another create attempt.
A deleted VM consumes capacity until GitHub runner cleanup is confirmed.

Regional cache defaults off. The app uses Runway Region for the cache endpoint
and CA, and Runway Cache Bucket to track buckets it owns. The region credential
can read `runway.api.cache_lease` and `runway.api.cache_bucket`. These methods
return raw JSON. Bucket lookup accepts a Team or tenant ID.

Run the Go contract tests against the app checkout:

```sh
RUNWAY_FRAPPE_APP=/path/to/runway-frappe-app go test ./...
RUNWAY_FRAPPE_APP=/path/to/runway-frappe-app python3 -m unittest discover -s scripts -p 'test_central_contract.py'
```

The tests skip when the app path is unset. The Go tests also skip when Python 3
is unavailable. The harness runs the app's transition and snapshot functions
with local storage, fencing and time substitutes. Run the app's integration
suite on a separate Frappe site to verify database behavior and authorization.
