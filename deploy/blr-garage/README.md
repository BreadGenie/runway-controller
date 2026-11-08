# Persistent regional cache on blr-1 (standalone)

Single-node Garage plus the `cache-data` service, running permanently on blr-1.
Garage is a cache, not a source of truth: a lost or damaged object is only a
cache miss. See the [standalone spec](../../specs/runway-regional-cache.md).

| Unit | Listens | Data |
| --- | --- | --- |
| `runway-garage.service` | S3 `127.0.0.1:3900`, RPC `127.0.0.1:3901`, admin `127.0.0.1:3903` | `/var/lib/runway-garage/{meta,data}` (root filesystem, replication 1) |
| `runway-cache-data.service` | TLS 1.3 `127.0.0.1:18445` | journal `/var/lib/runway-cache/state`, staging `/var/lib/runway-cache/spools` |
| `runway-cache-host@VM.service` | per-guest vsock sockets `/run/runway-cache/vms/VM/v.sock_{1026,1027}` | none (started by the [guest launcher](../standalone-guest/README.md)) |
| `runway-cache-assignments.service` | – (outbound scale-set message session) | records `/run/runway-cache/assignments/` |
| `runway-cache-health.timer` | – | runs the health check every 5 minutes |

Data lives on the root filesystem (`/dev/vda1`), not on the `metal` ZFS pool,
which belongs to metald. The cache-data budget caps the cache at 4 GiB in
total and 1 GiB per archive; the Garage layout declares 40 GiB.

## Files and secrets

- `garage.pin`: pinned Garage v2.3.0 URL and SHA256. `install.sh` refuses any other digest.
- `garage.toml`, `systemd/`, `tmpfiles.conf`: installed as-is.
- Generated on the host, root-only (0600), never in the repository or logs:
  `/etc/runway-garage/{rpc-secret,admin-token,metrics-token}`,
  `/etc/runway-cache/garage-key.json` (the bucket-scoped key),
  `/etc/runway-cache/cache-data.json` (it embeds that key), `/etc/runway-cache/host.json`,
  `/etc/runway-cache/signing-key.pem` (Ed25519 grant key), and `/etc/runway-cache/tls/cache-data.key`.
  The private CA key is discarded once the leaf is signed.
- `/etc/runway-cache/github-token` (0600): the host GitHub credential, which the
  launcher (JIT registration) and `cache-host` (job verification) use. It needs
  Actions read and self-hosted runner administration on the repository.

## Install

```sh
# On a workstation: build and copy.
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o runway .
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o cache-node ./cmd/cache-node
tar -cf - deploy/blr-garage deploy/standalone-guest integrations/actions-runner | ssh root@blr-1 'mkdir -p /root/runway-deploy && tar -C /root/runway-deploy -xf -'
ssh root@blr-1 'cat > /root/runway-deploy/runway' < runway
ssh root@blr-1 'cat > /root/runway-deploy/cache-node' < cache-node
# On blr-1:
bash /root/runway-deploy/deploy/blr-garage/install.sh /root/runway-deploy/runway /root/runway-deploy/cache-node
 # Keep the existing /etc/runway-cache/github-token during upgrades.
RUNWAY_SCALE_SET_ID=39 bash /root/runway-deploy/deploy/blr-garage/bootstrap.sh   # layout, bucket, key, TLS, configs; prints health
```

`bootstrap.sh` is idempotent. It keeps the existing key, TLS and signing key
when they are already present.

## Upgrade

- **runway**: rebuild, copy it, then rerun `install.sh`. It replaces the binary and restarts
  every binary consumer, including assignments and active cache-host instances. Restart recovery
  re-verifies every indexed object. Previous binaries remain as `*.previous`.
- **Garage**: change `garage.pin` (URL and SHA256, both from the official release), then rerun
  `install.sh` and `systemctl restart runway-garage`. Read the Garage release notes for metadata
  migrations first.
- **Signing key rotation**: delete `/etc/runway-cache/signing-key.pem`, rerun `bootstrap.sh` and
  restart `runway-cache-data`. Grants that are already outstanding stop working. Each lasts at most
  30 minutes.

## Wipe and rebuild

A wipe loses only cached data. Jobs miss and save again.

```sh
systemctl stop runway-cache-data runway-garage
rm -rf /var/lib/runway-garage/meta /var/lib/runway-garage/data /var/lib/runway-cache/state /var/lib/runway-cache/spools
rm -f /etc/runway-cache/garage-key.json
bash /root/runway-deploy/deploy/blr-garage/install.sh /opt/runway-cache/bin/runway /opt/runway-cache/guest/cache-node   # recreates the directories
bash /root/runway-deploy/deploy/blr-garage/bootstrap.sh
```

If only Garage is wiped and the cache-data journal is kept, startup hides every missing object as a
miss and keeps its accounting until deletion. An unreachable Garage is different: startup is
refused, and `systemd` retries until Garage answers.

## Health and usage

```sh
/opt/runway-cache/bin/runway-cache-health                 # one JSON line; non-zero on failure
systemctl list-timers runway-cache-health.timer; journalctl -u runway-cache-health -n 5
G="/opt/runway-garage/bin/garage -c /etc/runway-garage/garage.toml"
$G status; $G bucket info runway-cache                    # objects and bytes in the bucket
du -sh /var/lib/runway-garage/data; df -h /var/lib/runway-garage
python3 -c 'import json;e=json.load(open("/var/lib/runway-cache/state/state.json"))["Entries"] or [];print(len(e),"entries",sum(x["Size"] for x in e if x["Published"] and not x["Hidden"]),"published bytes")'
journalctl -u 'runway-cache-host@*' -o cat | grep '"event":"setup"'   # grants and refusals with reasons
```

The health check covers the following:

- Both units are active.
- Garage admin `/health` and `garage status` succeed.
- cache-data completes a TLS handshake under the private CA.
- Ports 3900, 3901, 3903 and 18445 are bound to 127.0.0.1 only.
- `timedatectl` reports NTP synchronized.
- The filesystem is under 90% full.

## Storage time budgets

`cache-data.json` accepts optional `storage_budget`: `base_seconds` (12),
`min_bytes_per_second` (16777216), `max_seconds` (300), and
`recovery_max_seconds` (900). Operations get base plus bytes/rate, capped at max;
startup shares base plus bytes/rate plus one second per object, capped at
recovery max. Preserve existing config, journal and Garage directories.

Standalone guests run through `/usr/bin/jailer` in the existing systemd unit,
using slot accounts `runway-fc-1` through `runway-fc-9`. Each jail lives at
`/srv/runway-jail/firecracker/VM/root`, with a kernel hardlink (copy across
filesystems), its own root-drive device node, and jail-relative config paths
`/vmlinux`, `/dev/rootfs` and `/vsock/v.sock`. The VM socket directory is
bind-mounted at `vsock/`, owned by root and the slot group with mode `0750`;
cache-host sockets use that group and mode `0660`. Binding v2 records
`jail_uid` and `jail_gid`: cache-host requires the peer PID, uid and gid to
match, and the gid must also match the socket directory. V1 bindings are refused.
The launcher verifies the jail's executable path and its digest against
`/usr/bin/firecracker` before writing the binding. Teardown, reap and failed
launch cleanup stop the unit, unmount `vsock/`, then remove the jail with
`rm -rf --one-file-system`. No extra PID namespace or jailer cgroup is used.

`deploy/blr-garage/install.sh` creates the nine system accounts (nologin, no
home) and the root-owned `0711` jail base. It refuses installation while any
binding file exists in `/run/runway-cache/bindings`; tear down guests first.
Rollback requires restoring both the launcher and runway `.previous` binaries.
