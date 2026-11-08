# Regional cache integration

The standalone service stores dependency archives in Garage on blr-1.
See the [spec](../../specs/runway-regional-cache.md) and
[deployment](../../deploy/blr-garage/README.md).

`pinned_clients.py` lists pinned releases, verifies offline upstream bundle hashes
and runs Node clients: actions/cache v4.3.0, v5.1.0 and v6.1.0, and
actions/setup-node v6.5.0 and v7.1.0. Exact commits are recorded alongside the
bundle hashes. Each release has its own directory under
`.scratch/runway-regional-cache/vendor/`; provision files from the recorded
full-SHA raw URLs before testing. Tests never fetch missing files and fail on
missing files or hash mismatches. Vendored files remain outside version control.
`test_relay_clients.py` exercises miss, save, exact and restore-key hits,
multiple upload chunks and byte equality through the guest relay. Each version
runs against fresh storage; cache main/post and restore-only/save-only pairs run
separately. setup-node exercises Yarn caching with `ACTIONS_CACHE_MODE` unset.
Every client connection must reach the fixed relay; ticket URLs are redacted.
Run it through `RUNWAY_CACHE_CLIENT_TEST=1 go test -race ./cache/transport -run TestPinnedClientsThroughRelay`.

`python3 -m unittest discover -s integrations/actions-cache` runs offline checks.
`RUNWAY_GARAGE_TEST=1 python3 integrations/actions-cache/test_garage_storage.py`
starts a disposable checksum-pinned Garage; it requires boto3 and a local binary.
It never reads deployment credentials or fetches bundles.

Go store tests own publication, quotas, interruption, recovery and corruption;
transport tests own credential isolation, TLS, deadlines and streaming bounds.
