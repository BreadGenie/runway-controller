# Regional cache validation

From the repo root:

```sh
gofmt -l .
go build ./...
go vet ./...
go test ./...
go test -race ./cache/... .
python3 integrations/actions-runner/test_cache_relay.py
CGO_ENABLED=0 go build -o /tmp/cache-node ./cmd/cache-node
python3 integrations/actions-cache/test_garage_storage.py
RUNWAY_CACHE_CLIENT_TEST=1 go test -race ./cache/transport -run TestPinnedClientsThroughRelay
RUNWAY_GARAGE_TEST=1 python3 integrations/actions-cache/test_garage_storage.py
```

The last two gates require local pinned client bundles and Garage respectively;
see [client prerequisites](../integrations/actions-cache/README.md).
Wrapper changes require Go wrapper and wire-v2 tests. The Metal runner retains
its separate 1025 administrative gate. Live qualification must preserve the journal, demonstrate warm hits
with two confirmed scale-set-39 jobs, and refuse forged runs, sibling UUIDs,
unbound guests and tag-as-branch claims. Tear down test guests and runners.
Repeated matched performance qualification and a standalone jailer remain open.
