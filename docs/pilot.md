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

This is trusted internal compute. Firecracker separation and guest-local Docker do not establish safe access for public fork PRs or arbitrary customer code. The current setup has not validated network isolation from control-plane services, other tenants, or cloud metadata. No customer dashboard, billing, usage ledger, shared cache, multi-host transport, or production host-drain policy is included.

Next, extend the verified restart and independent deadline checks with bootstrap response loss, ownership takeover, and GitHub outage cleanup. Measure queue-to-start and representative Frappe workflow performance before adding dependency or Docker caches. Keep cache trust and tenant boundaries explicit; reusable images must remain credential-free.
