# Serve container images to fresh guests from a loopback mirror

A Frappe workflow declares its databases as `services:` containers, so the runner pulls
those images on every job. This document records an anonymous, read-only registry mirror
on the Metal host, reached by a guest over one reverse SSH forward.

It changes no workflow, step, service or test. The images that run are the images the
workflow already names.

## Measured performance

| | `Initialize containers` |
| --- | --- |
| Runway, no mirror (baseline) | 45 s |
| Runway, mirror warm | 42 s |
| difference | 3 s |

**This is a single sample and it is provisional.** It is recorded because it is the only
end-to-end measurement taken, not because it is a result worth generalising.

The move was expected to be worth considerably more. Timestamps in the job logs show
that on GitHub-hosted every layer of all three images completes within about two seconds,
while on this hardware the same layers complete over thirteen, so the difference looked
like transfer time. A mirror removes the Docker Hub round trips and the transfer, and it
did not recover that time. The step still takes 42 s.

That means the transfer was not what dominates this step on this hardware, and this
change did not find out what is. Two readings remain open and neither is established
here: the remaining time is the guest unpacking and committing layers to its own disk,
which a mirror cannot avoid, or it is something else in the step that was never
measured. Distinguishing them needs a measurement of this step with and without the layer
transfer held separate, which has not been done.

Do not carry the earlier expectation forward. The 28-second figure came from arithmetic
on log timestamps, not from an experiment, and the experiment contradicts it.

## Design

Three ways to stop each job re-downloading the images were considered.

**Bake the images into the sealed guest image.** This removes job-time transfer and
unpacking, at the cost of a larger artifact. The separate baked-image experiment
uses `--preserve-container-images` while sealing a fresh builder; the default seal
still clears Docker state. See [image-building.md](image-building.md).

**A registry reachable from the guest over its own address.** Needs a stable address for
the Metal host inside every guest namespace, which this transport does not have: it
enters `metal-<vm-id>` for the connection and knows no host address there. It also opens
a listening port guest code can reach directly.

**A registry on the host's loopback, reached over one reverse SSH forward.** The
transport already opens an authenticated SSH connection into the guest on every action. A
reverse forward reuses it: the guest's `sshd` listens on the guest's own loopback and
hands each connection back along that connection to the client, whose loopback is the
Metal host's. No new listener, no new credential, no port on any interface the host has,
and no guest file. This is what is implemented.

### The mapping, and the mistake that makes it silently wrong

A reverse forward's local end is delivered to the **SSH client**. The transport's client
used to run inside the guest's namespace, because it reached the guest with
`ip netns exec metal-<vm-id> ssh ...`. From in there, `127.0.0.1` is the namespace's own
loopback, not the host's. A `-R` forward created from inside it would bind the right port
and deliver every connection to a loopback with nothing behind it: every check would pass
and no job would get a byte of cache.

The fix is to keep the client in the root namespace and enter the namespace only for the
TCP connection:

```
ssh -o ProxyCommand='ip netns exec metal-vm-123 socat - TCP:172.16.0.2:22' ...
```

`ProxyCommand` runs on the client, in the root namespace; only the `socat` inside it
sees the namespace. This changes every guest command, not just the forward, so `socat`
becomes a prerequisite on the Metal host.

## Operating it

### Prerequisites

`socat` on the Metal host, for every guest command:

```sh
command -v socat || apt-get install -y --no-install-recommends socat
```

The guest's `sshd` must permit remote forwarding, since it creates the listener:

```sh
grep -rE '^\s*AllowTcpForwarding' /etc/ssh/sshd_config /etc/ssh/sshd_config.d/ 2>/dev/null
```

No image rebuild is needed. `docker.io` is already in the baked image and brings
`dockerd`.

### Start and warm the mirror

Run on the Metal host as root, with no trial guest running:

```sh
bash scripts/prepare-container-cache.sh
```

It starts Docker's own `registry:2` as a pull-through cache of Docker Hub, published on
loopback only, warms the three tags the workflow declares, and verifies each digest
against the values a real job resolved:

| path | digest |
| --- | --- |
| `library/postgres:18.0` | `sha256:41fc5342eefba6cc2ccda736aaf034bbbb7c3df0fdb81516eba1ba33f360162c` |
| `library/mariadb:11.8` | `sha256:6422478cb8e159f080fb1d8ccf65101e26fe51385787fde7d16c3b165a331f15` |
| `rnwood/smtp4dev:3.7.1` | `sha256:9356c41b77175aa9dd530a8e1ded29cb2574f2d2474c2b0ab94f202763e6feb9` |

A tag that has moved reports `MOVED` and exits non-zero rather than caching something
else. `--update-digests` prints the observed values to adopt deliberately.
`--verify-only` re-checks without warming.

The three paths are the ones a Docker daemon actually asks for. The daemon normalises a
bare name before it contacts any registry, so `postgres:18.0` is requested as
`library/postgres`, while `rnwood/smtp4dev:3.7.1` keeps its own namespace. Warming the
wrong path would leave the guest's pull missing.

### Enable it for a pool

```json
"transport_command": [
  "python3", "/srv/runway/scripts/metal-guest-transport.py",
  "--host", "root@metal.example.com",
  "--key-file", "/var/lib/runway/guest-admin-key",
  "--known-hosts", "/var/lib/runway/known-hosts",
  "--registry-mirror-port", "5000"
]
```

It is off unless present. With it, `prepare` opens one reverse forward, proves the guest
can complete a registry request over it, and then configures the guest's Docker. `stop`
closes the forward and removes its control socket. `status`, `bootstrap` and `collect`
leave it alone, because those run while a job is live.

Preparation fails closed. A missing `socat`, a refused bind, a guest that cannot reach a
registry, a `dockerd` that will not validate the file, and a daemon that does not report
the mirror each stop preparation before any JIT configuration is requested.

### What the guest changes

Only during `prepare`, and only while it reports `installed`: a fresh clone with no
registration state, the same condition the passwordless-sudo opt-in uses. Nothing runs
after `.runner` exists, so a live job's Docker is never reconfigured.

Within that window it merges two keys into `/etc/docker/daemon.json`, preserving every
other key, validates the file with `dockerd --validate` before the daemon is pointed at
it, restarts Docker before it has served a container, and proves the restarted daemon
reports the mirror and answers as the `runner` account. A file that is a symlink, not a
regular file, not root-owned, or group- or world-writable is refused rather than
rewritten, and any failure restores the exact prior text.

It deliberately does not pull the images into the guest: a warm local store would make
the next pull free for reasons unrelated to the mirror, and would hide the effect being
measured. It writes no credential; the mirror is anonymous and the forward carries no
token.

### Forward lifecycle

One SSH control master per VM owns the forward, with its control socket in a private
`0700` directory beside the known-hosts prefix the controller already reserves:

```
ssh -M -S <dir>/forward.sock -f -N \
    -o ControlPersist=no -o ExitOnForwardFailure=yes \
    -o ServerAliveInterval=10 -o ServerAliveCountMax=3 \
    -o ProxyCommand='ip netns exec metal-vm-123 socat - TCP:172.16.0.2:22' \
    -R 127.0.0.1:5000:127.0.0.1:5000 root@172.16.0.2
```

`ServerAliveInterval` with `ServerAliveCountMax` bounds a master to about 30 s after a
guest stops answering; `ControlPersist=no` alone only releases it once the connection is
already gone. `ExitOnForwardFailure=yes` keeps a refused bind from becoming a
backgrounded success. A repeated `prepare` reuses the live master rather than opening a
second listener on a port it already owns.

Note for reviewers: `ssh -O check` and `ssh -O exit` require a destination and report a
usage error without one. Every control verb therefore names `root@172.16.0.2`.

## Trust boundary

The mirror has **no authentication** and is published on `127.0.0.1:5000` only, so it is
unreachable from the host's external interfaces. What that does and does not mean:

- **A guest cannot reach it directly.** The only path in is the reverse SSH forward,
  which exists for the life of one VM's lease and carries no token.
- **Any process on the Metal host can reach it.** A host-local process that can write to
  the registry's storage could substitute an image for a later guest. In practice this
  does not widen the boundary, because root on the Metal host creates the guest and
  writes its image and so already controls what the guest runs. It is still a real
  property, and a host admitting untrusted jobs alongside the mirror would need its own
  trust model settled first.
- **The cache is shared by every job on this host.** That is the same model as a shared
  Actions cache and needs the same scrutiny before untrusted jobs are admitted.
- **Content is verified, provenance is not.** Docker checks every blob against the
  manifest it fetched; the manifest itself comes from the mirror.

The preparation script never restarts or reconfigures the host Docker daemon, which is
shared with other services on this host. It uses the daemon only as a client for
`docker run` and `docker pull`; pulling from a loopback registry needs no daemon
configuration because Docker already treats `127.0.0.0/8` as insecure.

A mirror that is down, empty or corrupt does not fail a job. The daemon tries mirrors
ahead of Docker Hub and falls back to it on error, including on a missing manifest or
config blob. That makes an unreliable mirror a performance problem rather than an
availability one.

## Verifying it

The mirror, from the Metal host:

```sh
curl -sS -o /dev/null -w '%{http_code}\n' http://127.0.0.1:5000/v2/
ss -ltnH 'sport = :5000'          # must show 127.0.0.1 only
docker logs runway-registry-mirror --tail 20
```

That the host daemon was not touched and has no mirror configured:

```sh
docker info --format '{{json .RegistryConfig.Mirrors}}'   # must print null
```

Inside a live trial guest:

```sh
ip netns exec metal-<vm-id> ssh -i <guest-admin-key> root@172.16.0.2 \
  'curl -sS -o /dev/null -w "%{http_code}\n" http://127.0.0.1:5000/v2/'
ip netns exec metal-<vm-id> ssh -i <guest-admin-key> root@172.16.0.2 \
  'runuser -u runner -- docker info --format "{{json .RegistryConfig.Mirrors}}"'
ip netns exec metal-<vm-id> ssh -i <guest-admin-key> root@172.16.0.2 \
  'cat /etc/docker/daemon.json'
```

The first must print `200`, the second must list `http://127.0.0.1:5000/`, and the third
must show the two keys with any pre-existing key intact.

That no forward outlived its lease:

```sh
ls -la /var/lib/runway/runway-forward-*/
```

## Removing it

Drop `--registry-mirror-port` from the transport command. Guests return to Docker Hub
behaviour with no other change, because the flag is what creates both the forward and the
guest's configuration.

On the host, once no pool uses it:

```sh
docker rm -f runway-registry-mirror
docker volume rm runway-registry-cache
```

## Limits

- The 3-second figure is one sample on one host and is not a result to plan against.
- The remaining time in this step is unexplained. Anyone pursuing parity needs to
  separate the layer transfer from the unpack before choosing a next step.
- One pool, one host: the transport is a single-host administrative bridge.
- It is not image parity, not a promotion policy, not signed, and not hardened against a
  hostile host.

## See also

- [Image building](image-building.md) for the baked guest image, reused unchanged here
- [Internal pilot](pilot.md) for the transport, its opt-ins and its prerequisites
- [Proof of concept results](poc-results.md) for the recorded measurements
- `scripts/prepare-container-cache.sh` for the recipe
- `scripts/metal-guest-transport.py` for the forward and the guest configuration