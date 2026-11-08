# Regional cache integration

The standalone service stores dependency archives in Garage on blr-1.
See the [spec](../../specs/runway-regional-cache.md) and
[deployment](../../deploy/blr-garage/README.md).

`pinned_clients.py` reads the shared JSON pinned releases, verifies offline upstream bundle hashes
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


The routing source of truth is `cmd/cache-node/allowlist.json`: repository,
release, full commit, entrypoint and SHA256. setup-node's action.yml is
harness-only. cache-node embeds the file; invalid data disables all regional
routing without preventing Node execution. Non-candidates still perform no file I/O.

The daily and manually dispatched `Cache allowlist` workflow tracks every major
tag at or above the oldest pinned major, including newly published majors. It
ignores releases without a major tag, retains every tested pin, resolves manifests
and fetches bundles by full commit. It runs vet, race tests and every pinned relay
scenario before opening an idempotent `bot/cache-allowlist-<versions>` PR. Failures
appear in the run summary and produce no PR. Review and merge the PR, then rebuild
and deploy the wrapper through the normal deployment process.

Manual fallback from the repository root:

```sh
python3 -I integrations/actions-cache/update_allowlist.py --dry-run
python3 -I integrations/actions-cache/update_allowlist.py --fetch-dir /tmp/runway-cache-clients
RUNWAY_CACHE_VENDOR=/tmp/runway-cache-clients RUNWAY_CACHE_CLIENT_TEST=1 go test -race ./cache/transport -run TestPinnedClientsThroughRelay -v
```

The update command appends candidates; review its JSON diff and run all workflow
checks before committing. `--fetch-dir` provisions hash-checked files into a
chosen cache directory. Tests remain offline and use `RUNWAY_CACHE_VENDOR`, or
the existing gitignored `.scratch/runway-regional-cache/vendor/` by default.
Node, Yarn classic and zstd are required.

Enable repository Settings → Actions → General → Workflow permissions →
**Allow GitHub Actions to create and approve pull requests** for automated PRs.
The workflow grants contents/write and pull-requests/write only to its update
job and uses the automatic GITHUB_TOKEN; no additional secret is required.
Organization policy must permit these permissions. PRs created with GITHUB_TOKEN
do not trigger ordinary pull_request workflows; the bot runs its checks before
creating the PR. Required external checks may need a human-triggered run.
