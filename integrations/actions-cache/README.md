# Regional cache integration

The standalone service stores dependency archives in Garage on blr-1.
See the [spec](../../specs/runway-regional-cache.md) and
[deployment](../../deploy/blr-garage/README.md).

`pinned_clients.py` reads the shared JSON pinned releases, verifies offline upstream bundle hashes
and runs Node clients: actions/cache v4.3.0, v5.1.0 and v6.1.0, and
actions/setup-node v6.5.0 and v7.1.0, and astral-sh/setup-uv v6.8.0 and v7.6.0. Exact commits are recorded alongside the
bundle hashes. Each release has its own directory under
`.scratch/runway-regional-cache/vendor/`; provision files from the recorded
full-SHA raw URLs before testing. Tests never fetch missing files and fail on
missing files or hash mismatches. Vendored files remain outside version control.
`test_relay_clients.py` exercises miss, save, exact and restore-key hits,
multiple upload chunks and byte equality through the guest relay. Each version
runs against fresh storage; cache main/post and restore-only/save-only pairs run
separately. setup-node exercises Yarn caching with `ACTIONS_CACHE_MODE` unset.
setup-uv exercises its setup/restore and post/save bundles with a preseeded local uv tool cache,
explicit cache inputs and pruning disabled; both majors save and restore identical bytes.
Every client connection must reach the fixed relay; ticket URLs are redacted.
Run it through `RUNWAY_CACHE_CLIENT_TEST=1 go test -race ./cache/transport -run TestPinnedClientsThroughRelay`.

`python3 -m unittest discover -s integrations/actions-cache` runs offline checks.
`RUNWAY_GARAGE_TEST=1 python3 integrations/actions-cache/test_garage_storage.py`
starts a disposable checksum-pinned Garage; it requires boto3 and a local binary.
It never reads deployment credentials or fetches bundles.

Go store tests own publication, quotas, interruption, recovery and corruption;
transport tests own credential isolation, TLS, deadlines and streaming bounds.


The routing source of truth is `cmd/cache-node/allowlist.json`: repository,
release, full commit, entrypoint and SHA256. setup-node and setup-uv action.yml files are
harness-only. Candidate paths, allowlist keys and fallback notices use full owner/name
identities; only the enumerated repositories and entrypoint paths are accepted. cache-node embeds the file; invalid data disables all regional
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
Node, Yarn classic, uv and zstd are required.

Enable repository Settings → Actions → General → Workflow permissions →
**Allow GitHub Actions to create and approve pull requests** for automated PRs.
The workflow grants contents/write and pull-requests/write only to its update
job and uses the automatic GITHUB_TOKEN; no additional secret is required.
Organization policy must permit these permissions. PRs created with GITHUB_TOKEN
do not trigger ordinary pull_request workflows; the bot runs its checks before
creating the PR. Required external checks may need a human-triggered run.


setup-uv compatibility was checked with `gh api` against upstream major refs,
annotated tag objects, manifests and bundles. The published moving major refs
are v1–v7; the tracked newest two resolve to
[v6.8.0](https://github.com/astral-sh/setup-uv/tree/d0cc045d04ccac9d8b7881df0226f9e82c39688e)
and [v7.6.0](https://github.com/astral-sh/setup-uv/tree/37802adc94f370d6bfd71619e3f0bf239e1f3b78).
Later numbered releases without an @vN tag are excluded by the same policy as
the other clients. v6 uses `dist/setup/index.js` and `dist/save-cache/index.js`;
v7 uses the corresponding `index.cjs` files.
Both tagged bundles include v1 REST (`_apis/artifactcache/`) and v2 Twirp
implementations. Their `getCacheServiceVersion` selects v1 when
`ACTIONS_CACHE_SERVICE_V2` is unset, as it is on the regional route.
Only setup/restore and post/save are routed; the checksum-maintenance bundle
is not an allowed entrypoint. Hashes below are computed from full-commit raw
bytes, using the same fetch path as the bot.

| Release | File | SHA256 |
| --- | --- | --- |
| v6.8.0 | `dist/setup/index.js` | `d647c2b0399e4bb7cfbe8ed453d757a8e30d6fd42b8ca94b4f4ce8aae69ba769` |
| v6.8.0 | `dist/save-cache/index.js` | `d43ebf40c109d220db02f8bf166bdd900d7fc42bed043f1ddc59b7f15dac4265` |
| v6.8.0 | `action.yml` | `b7e64add830d513ed49b8cadba8c75138eb45e0ff3dbf95bd687cc250b761444` |
| v7.6.0 | `dist/setup/index.cjs` | `58b1a54d364b29926dc91f4f506050408e9348530dfc25edb758655c32f4ad13` |
| v7.6.0 | `dist/save-cache/index.cjs` | `993a087978d964d5b62c5bde21e5de2036b47b8c01b89311d4c2fd51dd58311d` |
| v7.6.0 | `action.yml` | `fb1672973d8ed83aa98a26f908b2946aafa01d415299913d55bb135ab0864752` |
