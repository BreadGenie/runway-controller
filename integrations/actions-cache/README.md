# Regional cache integration

The standalone service stores dependency archives in Garage on blr-1.
See the [spec](../../specs/runway-regional-cache.md) and
[data service](data-service.md).

`pinned_clients.py` verifies offline upstream bundle hashes and runs Node clients.
`test_relay_clients.py` exercises miss, save, exact and restore-key hits,
multiple upload chunks and byte equality through the guest relay.
Run it through `RUNWAY_CACHE_CLIENT_TEST=1 go test -race ./cache/transport -run TestPinnedClientsThroughRelay`.

`python3 -m unittest discover -s integrations/actions-cache` runs offline checks.
`RUNWAY_GARAGE_TEST=1 python3 integrations/actions-cache/test_garage_storage.py`
starts a disposable checksum-pinned Garage; it requires boto3 and a local binary.
It never reads deployment credentials or fetches bundles.

Go store tests own publication, quotas, interruption, recovery and corruption;
transport tests own credential isolation, TLS, deadlines and streaming bounds.
