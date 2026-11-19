# Runway Frappe app

Install `runway` on a Frappe site with only Frappe and Runway. It owns the
Runner Pool and Runner Lease DocTypes and serves `runway.api.*`. Point the
controller at the Runway site with a local Frappe API key. See the
[pilot setup](../../docs/pilot.md).

Set `runway_central_url` to the Central origin. Each pool links to its Team's
Runway Team Connection, which stores the Central service user's credential.
Choose a Central plan, image offering and regional image. Runway persists
the Central request key and original payload before create. Its scheduler
reconciles actions, lease expiry and server termination. A lease holds
capacity until both the VM and GitHub registration are confirmed absent.

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
