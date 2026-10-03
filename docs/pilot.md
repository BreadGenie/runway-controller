# Durable internal pilot

The pilot adds a Central-backed controller to the earlier operator execution tools. Central stores Runner Pools and Runner Leases; Runway observes GitHub demand, boots a fresh Atlas VM from a baked image, supplies a JIT runner configuration, and reconciles cleanup. Start with one trusted internal repository and concurrency one.

The controller and recovery tests are implemented. The baked image has booted as a fresh guest and passed a Docker Redis published-port/PING check without installing packages at boot. Earlier manually coordinated workflows passed on this infrastructure. A real workflow passed after a same-owner controller crash and restart. Central also reclaimed a separate expired VM with the controller stopped. See [results](poc-results.md); broader outage and ownership-takeover drills remain.

## Install the Central integration

Prepare the infrastructure described in [development](development.md). The [Central integration patch](../integrations/central/README.md) targets revision `6676d3178943d332c288fc55c04e6f44a8d01732`; it is not an upstream release. Review the patch before applying it to the Central checkout installed in your bench:

```sh
git -C /path/to/bench/apps/central apply --check /path/to/runway/integrations/central/central-runway-pilot.patch
git -C /path/to/bench/apps/central apply /path/to/runway/integrations/central/central-runway-pilot.patch
cd /path/to/bench
bench --site CENTRAL_SITE migrate
bench --site CENTRAL_TEST_SITE run-tests --app central --module central.tests.test_runway
bench --site CENTRAL_SITE enable-scheduler
```

Use a separate test site for the test command. Restart the site's web and worker processes after installation, run its worker queues, and configure the bench scheduler tick to ten seconds. Keep Atlas reconciliation running too. Read `spec/RUNWAY.md` in the patched checkout for the API and state-machine contract.

Create a dedicated API-key service user with `Central User` and a team-wide Developer or suitable custom role granting `server:view`, `server:create`, and `server:terminate`. Assign it as the pool's `controller_user`; do not give it System Manager. Store its `api_key:api_secret` in a private regular file, mode `0600`, outside the checkout.

## Prepare the pool

Publish an Available System image using the [baked runner image recipe](image-building.md). It includes the official runner, Docker, and a compatible Firecracker kernel; it must contain no registration, JIT material, SSH host identity, or operator credentials. Verify a fresh clone before selecting the image for a pool. The installer is an image-build step, not a controller boot step.

Create a dedicated scale set with an existing administrator credential or GitHub App configuration described in [development](development.md):

```sh
go build -o runway .
./runway --action create --url https://github.com/OWNER/REPO --name runway-dogfood
```

Record the returned ID. An uncertain create response requires remote inspection before another create. In Central Desk, an operator creates a Runner Pool with the following values:

| Field | Initial setting |
| --- | --- |
| Team and region | Authorized Team and healthy enrolled region |
| Controller user | Dedicated service user |
| Repository URL and scale-set ID | The repository and dedicated scale set above |
| Image ID and SSH public key | Your baked image and guest administrative public key |
| CPU, memory, disk | Start with 1000 millicores, 2048 MiB RAM, 4096 MiB disk; size for your workflow |
| Max concurrency | `1`, the supported controller pilot limit |
| Max job seconds | Total lease lifetime, including provisioning; for example `1200` |
| Status | Paused until configuration is ready, then Active |

Central additionally caps admission at four unfinished leases per Team. A lease consumes capacity until both the VM and GitHub registration are confirmed absent. The tested 4 GiB guest disk is tight for larger workloads.

## Configure and run

The supplied transport is an operator bridge for a single known Metal host. It uses SSH to enter `metal-VM_ID` network namespaces and SSH into the guest at its namespace-local address. The host must have the guest administrative private key corresponding to the pool's public key. Establish host SSH trust before launch. Placement must stay on that host; the transport does not discover a guest's host in a multi-host region.

Create a private configuration file using deployment-specific absolute paths:

```json
{
  "central_url": "https://central.example.com",
  "central_credential_file": "/srv/runway/private/central.token",
  "pool": "YOUR_POOL_ID",
  "transport_timeout_seconds": 90,
  "transport_command": [
    "python3", "/srv/runway/scripts/metal-guest-transport.py",
    "--host", "root@metal.example.com",
    "--key-file", "/var/lib/runway/guest-admin-key",
    "--known-hosts", "/var/lib/runway/known-hosts",
    "--diagnostics-dir", "/srv/runway/private/diagnostics"
  ]
}
```

`key-file` and `known-hosts` are paths on Metal; `diagnostics-dir` is local to the controller. Known-host files are separated by VM ID. The transport requires Python 3 on the controller, Metal and guest, plus working SSH and `ip netns` on Metal. Keep configuration and diagnostic directories private. Diagnostics can contain workflow output and must be handled as sensitive logs.

`central_url` must use `https`: every call sends the service user's `api_key:api_secret` in an `Authorization` header, so plaintext HTTP is refused except for a literal `localhost`, `127.0.0.1` or `[::1]` endpoint. Hosts are matched after parsing, so lookalikes such as `localhost.example.com` and `127.0.0.1.example.com` are refused, as are userinfo, query strings, fragments and unspecified addresses like `0.0.0.0` and `[::]`. Central must not redirect to another host; a redirect is reported as a rejected call. To test against a remote Central, forward a port to a loopback backend on the Central host and point `central_url` at it:

```sh
ssh -N -L 127.0.0.1:8003:127.0.0.1:8000 operator@central.example.com
```

Set `"central_url": "http://127.0.0.1:8003"` and, when the backend routes by virtual host, `"central_host": "central.example.com"` to match the configured site. Never forward to a non-loopback backend port.

The controller uses the same GitHub authentication as the other CLI commands: the current `gh` credential, or `RUNWAY_GITHUB_APP_CLIENT_ID`, `RUNWAY_GITHUB_APP_INSTALLATION_ID`, and `RUNWAY_GITHUB_APP_PRIVATE_KEY_FILE`. GitHub App and Central credentials remain outside guests. JIT material travels through stdin to the guest and is consumed by its runner wrapper.

```sh
./runway --action controller --config /srv/runway/private/controller.json
```

Leave `owner_id` unset so each process gets a fresh identity. Only one process owns the pool at a time. Claims are fenced in Central; ownership renews while the process runs. Configure service supervision to restart on failures, allowing the existing owner lease to expire before takeover when necessary.

After `controller_listening`, dispatch a trusted workflow using the dedicated `runs-on` label. Start with the [Docker smoke workflow](../examples/smoke-workflow.yml). Inspect Central's Runner Lease, the GitHub run, controller health output, and collected diagnostics. GitHub remains the authority for job success; a stopped guest service does not prove success.

### Sudo compatibility for trusted workflows

Some trusted GitHub Actions workflows expect a passwordless `sudo`. Preparation can grant it to the guest's `runner` account, but only when an operator asks for it. Append `--allow-guest-sudo` to `transport_command`:

```json
"transport_command": [
  "python3", "/srv/runway/scripts/metal-guest-transport.py",
  "--host", "root@metal.example.com",
  "--key-file", "/var/lib/runway/guest-admin-key",
  "--known-hosts", "/var/lib/runway/known-hosts",
  "--diagnostics-dir", "/srv/runway/private/diagnostics",
  "--allow-guest-sudo"
]
```

The flag is off by default, and without it `prepare` performs its existing checks only and writes nothing. With it, `prepare` — and only `prepare`, and only while the guest still reports `installed`, an unregistered fresh clone — writes one static, root-owned `0440` drop-in at `/etc/sudoers.d/runway-runner` holding `runner ALL=(ALL) NOPASSWD: ALL`. Nothing is installed or downloaded: the baked image must already carry `sudo` and `visudo`. The file is created fresh, never written through an existing file or symlink, parsed with `visudo -c -f`, and then proved with `runuser -u runner -- sudo -n true`. A missing binary, a planted drop-in or any failed check fails preparation closed: no JIT configuration is requested and no bootstrap follows, so the controller surfaces the error until the lease deadline retires it. Status, collect, stop and bootstrap never touch the file, and a guest that already carries a registration is never configured.

That root is optional and it is the disposable Firecracker guest's own root. It grants nothing on the Metal host, in the `metal-VM_ID` namespace, on Atlas or Central, and it reaches no GitHub App key, Central credential or host key. Keep the flag off while a pool could run untrusted code, including public fork pull requests.

### Workspace layout and artifact cache compatibility

Runway requests the work folder `/home/runner/work` as an **absolute** path when it generates a JIT runner config, instead of the runner-relative `_work` default. The runner accepts this: `RunnerSettings.WorkFolder` is an unvalidated string, and `HostContext` resolves the work directory with `Path.Combine(root, workFolder)`, which returns a rooted second operand verbatim, so the absolute value is used exactly as sent. The controller asks for one layout only so that artifacts cached under one runner type restore under the other.

This is not cosmetic. `actions/setup-node` with `cache: yarn` archives the yarn store as paths **relative to the checkout workspace**, so the `../../` prefix is baked into every member of the tarball. The relative `_work` default resolves under the runner install root, which places the checkout one directory deeper than GitHub-hosted (`$HOME/actions-runner/_work/<repo>/<repo>` versus `$HOME/work/<repo>/<repo>`). The two layouts therefore disagree by exactly one `../`. A cache entry written under one layout and restored under the other resolves to a directory that does not exist; `tar` does not create parents, so **every** member fails with `Cannot mkdir`. The failure is reported only as a `##[warning]Failed to restore` plus a subsequent `yarn cache is not found`, and the step still concludes `success` — so the job goes green while silently losing the cache and refetching every package. Because the key `node-cache-<os>-<arch>-<pm>-<lockfile-hash>` encodes no workspace depth, the incompatible entry is shared by both runner types rather than namespaced away from them.

After this change both runner types emit the same prefix and can read each other's cache entries. A cache archive already written under the old relative layout is incompatible on both sides and must be cleared before the alignment means anything; clearing it is an operator action, not something the controller does. Note also that a pre-populated yarn cache on disk does not skip the restore: `setup-node` still performs the restore step whenever `cache: yarn` is set, so locally present files cannot be relied on to remove the transfer. Do not bake job-produced dependency state into the image and do not introduce a registry or cache proxy to work around this; the workspace layout is the only variable to correct.

Scope: this fixes the path geometry for guests this controller provisions. It does not migrate existing cache entries, does not affect GitHub-hosted runners in any way, and does not alter the number or order of job steps. Each disposable Firecracker guest starts from the baked image, so the layout holds for every job without carried-over state. `controller_workspace_test.go` pins both the literal path and the workspace-depth invariant that explains it.

## Recovery and shutdown

The controller commits lease, acquisition and JIT intent before remote side effects. An uncertain Atlas create is discovered by durable tags, never blindly repeated. An uncertain JIT registration is retired by its stable runner name. An interrupted bootstrap is observed before deciding whether to retire it; credentials are not resent.

An abrupt process death can leave GitHub's message session active. The pinned SDK creates a new session on startup and exposes no takeover API; normal shutdown closes the known session. During the live SIGKILL drill, session creation recovered after roughly one minute. This is an observed delay, not a guaranteed expiry interval. Runway retries connection, continues Central ownership renewal and lease reconciliation, and reports unhealthy until it reconnects. Existing lease deadlines are not extended; Central's reaper and the guest runtime limit remain active. Allow this recovery delay within the job deadline, and do not delete a scale set to force reconnection.

Central's scheduled reaper expires VMs even while Runway is unavailable. Runway must return to remove GitHub registrations. An undiscovered uncertain create deliberately retains capacity for operator investigation. Neither a successful DELETE response nor a stopped process proves resources are absent.

Pause the pool to stop admission and retire existing leases; pausing does not gracefully finish running jobs. Keep the controller, Central workers and Atlas reconciliation running until leases show VM phase `deleted` and JIT state `removed`. Then stop the controller. Remove a dedicated scale set only after cleanup:

```sh
./runway --action delete --url https://github.com/OWNER/REPO --id SCALE_SET_ID
```

Pool identity changes require a drained pool and a controller restart. Preserve lease history and sanitized evidence. Do not clear an uncertain record merely to recover its admission slot.

## Limits and next validation

This is trusted internal compute. Firecracker separation and guest-local Docker do not establish safe access for public fork PRs or arbitrary customer code. The current setup has not validated network isolation from control-plane services, other tenants, or cloud metadata. No customer dashboard, billing, usage ledger, multi-host transport, or production host-drain policy is included.

Cache entries still live in GitHub's own Actions cache service. Aligning the workspace layout fixed the yarn cache, which one runner type had been writing and the other unable to restore, and Runway operates no regional cache tier and no proxy or mirror in front of that service.

Next, extend the verified restart and independent deadline checks with bootstrap response loss, ownership takeover, and GitHub outage cleanup. Performance has been measured on a real Frappe integration job; the next step is a benchmark with hardware matched to GitHub-hosted, then trials of a regional dependency cache and an OCI image cache. Keep cache trust and tenant boundaries explicit; reusable images must remain credential-free.

### Clock reads and the guest clocksource

Firecracker passes the guest no pvclock page, so the guest kernel's `kvm-clock` is MSR-backed and the vDSO has nothing to read in userspace. Every `clock_gettime` in every process on the guest therefore becomes a full syscall. Measured on this pilot's guest, `time.monotonic()` cost 205 ns per call against 49 ns on a GitHub-hosted runner, and `strace` counted 470 `clock_gettime` syscalls for 20 calls. Selecting `tsc`, which the same kernel already lists in `available_clocksource`, dropped that to 57 ns and to zero syscalls.

Preparation can select it, opt-in, and only while the guest still reports `installed` on an unregistered fresh clone. Append `--prefer-guest-tsc` to `transport_command`:

```json
"transport_command": [
  "python3", "/srv/runway/scripts/metal-guest-transport.py",
  "--host", "root@metal.example.com",
  "--key-file", "/var/lib/runway/guest-admin-key",
  "--known-hosts", "/var/lib/runway/known-hosts",
  "--diagnostics-dir", "/srv/runway/private/diagnostics",
  "--prefer-guest-tsc"
]
```

The flag is off by default, and without it `prepare` performs its existing checks only and writes nothing. With it, `prepare`, and only `prepare`, and only while the guest still reports `installed`, selects `tsc` and reads the attribute back to confirm it took. A guest already on `tsc` is confirmed and left alone, so a repeated preparation does not write again. A kernel that does not list `tsc` in `available_clocksource`, a kernel that has marked its TSC unstable, a missing attribute, and an attribute that is not a root-owned regular file are each refused, and preparation fails closed with the transport's generic `unknown` state, so no JIT configuration is requested and no bootstrap follows. Inspect `/sys/devices/system/clocksource/clocksource0/` when diagnosing a refusal.

Nothing is installed or downloaded, and two preconditions make selecting a clocksource as root responsible rather than merely fast. The kernel must advertise `tsc` in `available_clocksource`, and it must not have marked its own TSC unstable by creating an `unstable` attribute in the same directory. Preparation refuses a guest that fails either, and this guest's kernel passes both. That is the whole of the risk: a root-selected `tsc` on a kernel that has declared its TSC unstable trades clock accuracy for latency, which is the opposite of what this opt-in is for, so it is a refusal rather than a judgement call. A weaker position than the sudo opt-in above, which grants privilege: selecting a clocksource grants none. The attributes are root-owned and not group- or world-writable, which is what the kernel exposes and what preparation verifies rather than assumes; anything else, including a symlink, is refused. The write therefore needs root, which is why this lives in `prepare` and not in the job. The setting is the disposable guest's own kernel state. It reaches no Metal host value, no network namespace, no control-plane or GitHub credential, and it is applied before registration so no job observes a partial change. `bootstrap`, `status`, `collect` and `stop` never touch it, and a guest that already carries a registration is never reconfigured. A refusal from the opt-ins after this one leaves it applied, which this code cannot undo; preparation is idempotent, so the retry re-verifies rather than rolls back.

This is a microbenchmark result on an unloaded guest, with no job and no controller running. It closed about 95% of the guest's excess over a GitHub-hosted runner on clock reads, from 205 ns against 49 ns to 57 ns, and moved nothing else measurably in the same session. It has not been measured end to end on a job, so it is not a prediction about the 708-test phase. See [Frappe performance](frappe-performance.md) for the full per-operation comparison, for what this does not fix, and for the larger per-operation cost that no Runway-side change reaches.
