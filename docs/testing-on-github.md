# Testing a change on real GitHub Actions

This is the end-to-end loop: make a change in `runway`, run it against a real repository
pushing real commits, and see a job land on a guest you built. It is the only test that
proves the product promise, which is that a repository changes its `runs-on` line and nothing
else.

Nothing here is a unit test. Everything here is a control plane, a VM, and GitHub.

## What has to be true before you start

Four things, all outside this repo:

| Thing | How to check | Currently |
| --- | --- | --- |
| Central services up | `python3 ../scripts/fc-services.py status` | `central-web`, `atlas-web`, `worker`, `scheduler`, `mariadb`, `redis-*`, both metal tunnels |
| A Runner Pool, Active | `tabRunner Pool` where `status='Active'` | `lnc22b893b`, scale set `runway-2vcpu-6gb` (id 27) |
| A scale set with that label | GitHub → repo → Settings → Actions → Runner scale sets | `runway-2vcpu-6gb`, group 1 |
| Host RAM for the shape | `free -m` on the Metal host | blr-1 has ~7.1 GiB free; a 6 GiB guest fits, an 8 GiB guest does not |

**Check host RAM first.** It is the single most common false start. Atlas refuses VM
creation with "No Metal Server has capacity" when the guest will not fit, and that surfaces
as a wall of failed leases rather than one clear message. `free -m` on the Metal host, and
`sudo ip netns list` to see whether a stranded VM is holding memory. A lease goes stale, its
VM stays up, and the next hundred VMs all fail.

## The loop

### 1. Build the controller

```sh
go build -o /tmp/runway-poc .
```

Build to a path outside the repo. You will be running this continuously and you do not want
to rebuild the binary someone else might be using.

### 2. Start the controller

```sh
HTTPS_PROXY=socks5h://127.0.0.1:19080 \
  /tmp/runway-poc --action controller --config /tmp/runway-config.json
```

Two constraints on how you run it:

- **`HTTPS_PROXY=socks5h://127.0.0.1:19080` is required.** The controller talks to the
  GitHub Actions service through the tunnel. Without it the claim fails.
- **It must survive your shell.** `setsid nohup ... </dev/null >/tmp/controller.log 2>&1 &`
  or it dies when the tool call that started it returns, and you will read that as a bug.

The config:

```json
{
  "central_url": "http://127.0.0.1:8003",
  "central_credential_file": "/path/to/central.token",
  "pool": "lnc22b893b",
  "transport_timeout_seconds": 90,
  "transport_command": [
    "python3", "/home/t3code/projects/exp/runway/scripts/metal-guest-transport.py",
    "--host", "root@blr-1.sfu.frappe.cloud",
    "--key-file", "/var/lib/fc-metal/keys/id_ed25519",
    "--known-hosts", "/var/lib/fc-metal/known-hosts",
    "--allow-guest-sudo", "--prefer-guest-tsc",
    "--diagnostics-dir", "/tmp/runway-diagnostics"
  ]
}
```

`central_url` must be loopback or the controller refuses plaintext HTTP. The credential is
`api_key:api_secret` for the pool's `controller_user`; read it from the site, do not paste it
into a committed file.

A successful start prints:

```json
{"event":"controller_claimed","fence":11,"owner_id":"...","pool":"lnc22b893b"}
{"event":"controller_listening","pool":"lnc22b893b"}
```

`claimed` means it owns the pool. A `417` on claim means another controller still holds it —
that is fencing working, not a bug.

### 3. Make the pool take work

The pool must be `Active`. **A paused pool is valid and will not be given work** — Central
refuses `ensure_vm` for a paused pool, so every lease fails with the reason visible only in
the lease row.

### 4. Trigger a job

In the fork branch, push anything:

```sh
git commit --allow-empty -m "ci: trigger a shape check" && git push
```

**Do not use `workflow_dispatch` unless the workflow is on the default branch.** It 404s
otherwise, which costs you a debugging cycle to work out. The `Runway Shape` workflow uses a
`push` trigger on the fork branch for exactly this reason.

### 5. Read what happened

For controller changes, also run the fork's **Server** workflow using its existing
environment setup and test commands. A shape check proves runner selection; it
does not exercise the Frappe test suite. Check that the matrix includes every
shard: `TOTAL_BUILDS: 2` requires `index: [1, 2]`. Use `max-parallel: 1` when the
host can fit only one guest. Keep coverage enabled and do not exclude failing tests.

When the Server workflow exists on the default branch, dispatch it against the
fork branch containing the runner label and full shard matrix:

```sh
gh workflow run server-tests.yml -R BreadGenie/frappe --ref YOUR_TEST_BRANCH \
  -f runner=runway-2vcpu-6gb -f guest-caps=false
```

Wait for both integration jobs and coverage uploads, and report each shard's
test count, failures and errors. The MariaDB Server workflow does not cover the
separate UI or PostgreSQL workflows.

The job log is the primary evidence. `gh run view <id> --log` and grep for the shape lines:

```sh
gh run view <id> --log | grep -oE "runner:  [a-z0-9-]+|cpus: +[0-9]+|memory: +[0-9.]+ MiB|work: +[/a-z.-]+"
```

A passing run looks like:

```
runner:  runway-b6087fcc6c71411992441118cb4c3351
cpus:    2
memory:  5954.23 MiB
work:    /home/runner/work/frappe/frappe
```

`work:` matters as much as the CPU count. The image's baked bench lives at
`/home/runner/work/frappe/frappe`; a guest registered with a different work folder cannot
restore it, and the job fails with a message about bench that names neither cause.

The Central lease row is the second half of the evidence:

```sql
select name, phase, acquire_state, request_id is not null has_req,
       job_id is not null has_job, repository, request_labels,
       resolved_cpu_millicores cpu, resolved_memory_mib mem, vm_id, last_error
from `tabRunner Lease` where pool='lnc22b893b' order by creation desc limit 2;
```

`resolved_*` is the shape the job was admitted to. `repository` and `request_labels` are what
the pairing check sees. `vm_id` proves a guest was built.

### 6. Clean up

Pause the pool. An Active pool with a queued job will provision, and provisioning on
reported demand alone created and destroyed a VM every few seconds at one point in this
project's history. A paused pool costs nothing.

If you created VMs by hand while probing, terminate them. Check with
`sudo ip netns list` on the Metal host and compare against `metal/vms` in ZFS.

## Failure modes, in the order they will bite you

**`queued` / "waiting for a runner to pick up this job", controller healthy.**
The job is real, the controller is polling, and nothing goes wrong. First check host RAM:
if Atlas is refusing every VM, the guest never exists, and no runner is ever registered. The
controller only registers a runner from the VM it builds, so "no capacity" and "no runner"
look identical from the outside.

**`Atlas rejected VM creation`, on every lease.** Same cause: no capacity. `free -m` on the
host.

**A wall of leases, none with a job.** Proactive runner creation is how the scale set
advertises it can take work; it is not waste. If leases pile up without jobs, look for a
lease that fails and is replaced every few seconds, and read `last_error`.

**`417` or `508` from Central.** `417` is expectation failed on `claim_pool` and means another
controller already owns the pool — that is fencing working. `508` is Frappe's "a concurrent
transaction is blocking this one, retry later", a lock held past its wait timeout. The
controller retries it as a delay; a renewal that hits it still fails closed.

**`guest prepare failed or timed out` then recovered.** Normal. A single occurrence that
clears is the guest booting, not a failure. A repeating one is real.

**The suite passes except `test_rq_pool_idle_cpu_usage`.** That assertion expects idle CPU
under 10% and measures higher on a two-vCPU guest. It is the workload being sized for a
bigger box, not a defect in the runner.

## What the workflow should assert

A shape workflow that only echoes `nproc` proves nothing. Assert the machine against what the
pool promised:

```sh
cpus=$(nproc)
mem=$(awk '/MemTotal/ {print $2/1024}' /proc/meminfo)
[ "$cpus" -ge 2 ] || { echo "::error::expected at least 2 vCPU"; exit 1; }
[ "${mem%.*}" -ge 5 ] || { echo "::error::expected at least 5 GiB"; exit 1; }
```

Green on the wrong machine is the failure this exists to catch, and it is the same class of
bug as a bench restored into the wrong workspace.

## What this does not cover

- Registering a new repository, pool, scale set, or image: see `pilot.md`.
- The repository-name to image pairing guard has never refused a real job. The fields now
  flow, but the refusal path is unit-tested only. Verify it deliberately before trusting it.
- Anything on the spare VM host: it cannot run Metal, because its kernel will not load the
  mesh BPF program and its custom loader will not boot a newer one.
