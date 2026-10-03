#!/usr/bin/env bash
# Prepare an anonymous, read-only container image mirror on this Metal host.
#
# Run as root on the Metal host after the Frappe workflow's service images are known.
# It publishes Docker's own registry on this host's loopback only, caches the exact
# tags the unchanged workflow declares, and proves each cached digest. Guests reach it
# through one reverse SSH forward; see docs/container-cache.md.
#
# It never restarts or reconfigures this host's Docker daemon. The daemon is only ever
# used as a client, because Docker already treats a 127.0.0.0/8 registry as insecure and
# so needs no configuration to pull from a mirror on loopback. No workflow, step,
# service, test or guest image is touched.
set -euo pipefail
set +x

readonly CONTAINER=runway-registry-mirror
readonly VOLUME=runway-registry-cache
readonly IMAGE=registry:2
# The guest side must be given the same port through the transport's
# --registry-mirror-port, or the reverse forward and the guest's daemon disagree.
PORT=5000
# The v2 accept list a Docker daemon sends. It decides whether the registry answers with
# a multi-platform index or a single-platform manifest, and therefore which digest
# Docker-Content-Digest reports. Asking for both keeps this check comparable with the
# digest the guest's own pull will resolve.
readonly ACCEPT='application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.v2+json, application/vnd.oci.image.manifest.v1+json'
# The mirror path a Docker daemon uses for each image. The official images are namespaced
# under library/ because the daemon normalises a bare name before it asks any registry
# for it; rnwood/smtp4dev keeps its own namespace. Warming the wrong path would leave
# the guest's pull missing, so these are the paths that get warmed.
readonly MIRROR_PATHS=(
  'library/postgres:18.0'
  'library/mariadb:11.8'
  'rnwood/smtp4dev:3.7.1'
)
# Digests measured from a real Runway job's own `docker pull` output on 2026-10-02, and
# identical to the digests the same run resolved on GitHub-hosted runners. A tag that has
# moved since is a change to what the workflow runs, so it stops this script instead of
# quietly caching something else.
declare -A EXPECTED=(
  [library/postgres:18.0]=sha256:41fc5342eefba6cc2ccda736aaf034bbbb7c3df0fdb81516eba1ba33f360162c
  [library/mariadb:11.8]=sha256:6422478cb8e159f080fb1d8ccf65101e26fe51385787fde7d16c3b165a331f15
  [rnwood/smtp4dev:3.7.1]=sha256:9356c41b77175aa9dd530a8e1ded29cb2574f2d2474c2b0ab94f202763e6feb9
)

fail() { echo "ERROR: $*" >&2; exit 1; }
note() { echo "==> $*"; }

mode=prepare
update_digests=false
allow_live_guests=false
while [[ $# -gt 0 ]]; do
  case "$1" in
    --verify-only) mode=verify; shift ;;
    --update-digests) update_digests=true; shift ;;
    --allow-while-guests-live) allow_live_guests=true; shift ;;
    --port)
      PORT=${2:-}
      [[ $PORT =~ ^[0-9]+$ ]] && (( PORT >= 1024 && PORT <= 65535 )) ||
        fail 'Registry mirror port must be between 1024 and 65535.'
      shift 2
      ;;
    -h|--help) awk 'NR>1 && !/^#/ {exit} NR>1 {print substr($0,3)}' "$0"; exit 0 ;;
    *) fail "Unknown argument: $1" ;;
  esac
done

(( EUID == 0 )) || fail 'Run as root on the Metal host.'
for binary in docker curl; do
  command -v "$binary" >/dev/null 2>&1 || fail "Missing $binary."
done
docker info >/dev/null 2>&1 || fail 'The Docker daemon on this host is not answering.'

# Warming adds network and disk load on the same host that runs the benchmark, so it is
# refused while any trial guest namespace exists. A measurement taken against a host
# that was warming a cache measures the warming.
if ! $allow_live_guests; then
  live=$(ip netns list 2>/dev/null | awk '{print $1}' | grep -c '^metal-vm-' || true)
  (( live == 0 )) || fail "$live trial guest namespace(s) exist. Finish the run, or pass --allow-while-guests-live and accept that the result is perturbed."
fi

# Nothing else may own this loopback port. Binding over an unknown listener would both
# hide it and leave the mirror's contents unaccounted for.
mirror_state() {
  # absent, running, or stopped. Only a container this script created is ever reused.
  local id
  id=$(docker ps -aq --filter "name=^/${CONTAINER}$")
  if [[ -z $id ]]; then echo absent; return; fi
  if [[ $(docker inspect -f '{{.State.Running}}' "$id") == true ]]; then echo running; else echo stopped; fi
}

verify_owned_config() {
  # A stopped container of ours is only restarted once its own published port and proxy
  # setting are confirmed, so this script can never start a mirror it did not configure.
  local id=$1
  docker inspect -f '{{range .HostConfig.PortBindings}}{{range .}}{{.HostIp}}:{{.HostPort}}{{end}}{{end}}' "$id" |
    grep -qx "127.0.0.1:$PORT" ||
    fail "$CONTAINER publishes something other than 127.0.0.1:$PORT. Inspect it with: docker inspect $CONTAINER"
  docker inspect -f '{{range .Config.Env}}{{println .}}{{end}}' "$id" |
    grep -qx 'REGISTRY_PROXY_REMOTEURL=https://registry-1.docker.io' ||
    fail "$CONTAINER is not a pull-through cache of Docker Hub. Inspect it with: docker inspect $CONTAINER"
}

if [[ $mode == prepare ]]; then
  case "$(mirror_state)" in
    absent) ;;
    running) note "Reusing the running $CONTAINER." ;;
    stopped)
      note "Found a stopped $CONTAINER from an earlier run."
      verify_owned_config "$CONTAINER"
      note 'Starting it again.'
      docker start "$CONTAINER" >/dev/null
      ;;
  esac
fi

if ss -ltnH "sport = :$PORT" 2>/dev/null | grep -q . && [[ $(mirror_state) != running ]]; then
  fail "127.0.0.1:$PORT is already bound by something this script does not own. Find it with: ss -ltnp 'sport = :$PORT'"
fi

served_digest() {
  # The digest this registry resolves for a path, read from the registry's own answer.
  # Asking the registry rather than the local image store keeps this comparable with the
  # guest, which resolves the same header over the same reverse forward.
  local path=${1%:*} tag=${1##*:}
  curl -fsS -o /dev/null -D - -H "Accept: $ACCEPT" \
    "http://127.0.0.1:$PORT/v2/$path/manifests/$tag" 2>/dev/null |
    tr -d '\r' | awk 'tolower($1) == "docker-content-digest:" { print $2 }'
}

if [[ $mode == prepare && $(mirror_state) == absent ]]; then
  note 'Starting the mirror on this host'"'"'s loopback only.'
  docker volume create "$VOLUME" >/dev/null
  # REGISTRY_PROXY_REMOTEURL makes this a pull-through cache of Docker Hub, so the
  # daemon needs no static seed and a tag published later is picked up on first use.
  # Publishing to 127.0.0.1 keeps it off every interface this host has, which is what
  # makes an unauthenticated mirror acceptable here. Deletion is refused so the cache
  # cannot be emptied by anything but an operator removing the volume.
  docker run -d --name "$CONTAINER" --restart unless-stopped \
    -p "127.0.0.1:$PORT:5000" \
    -v "$VOLUME:/var/lib/registry" \
    -e REGISTRY_HTTP_ADDR=0.0.0.0:5000 \
    -e REGISTRY_PROXY_REMOTEURL=https://registry-1.docker.io \
    -e REGISTRY_STORAGE_DELETE_ENABLED=false \
    "$IMAGE" >/dev/null
fi

if [[ $mode == prepare ]]; then
  for _ in $(seq 1 30); do
    [[ $(curl -fsS -o /dev/null -w '%{http_code}' "http://127.0.0.1:$PORT/v2/" || true) == 200 ]] && break
    sleep 1
  done
  [[ $(curl -fsS -o /dev/null -w '%{http_code}' "http://127.0.0.1:$PORT/v2/") == 200 ]] ||
    fail "The mirror is not answering on 127.0.0.1:$PORT. Check: docker logs $CONTAINER"
fi

[[ $(curl -fsS -o /dev/null -w '%{http_code}' "http://127.0.0.1:$PORT/v2/") == 200 ]] ||
  fail "No registry is answering on 127.0.0.1:$PORT. Run without --verify-only first."

if [[ $mode == prepare ]]; then
  for reference in "${MIRROR_PATHS[@]}"; do
    note "Warming 127.0.0.1:$PORT/$reference through this host's Docker daemon."
    # Pulling from the mirror, not from Docker Hub, is what warms it: the daemon
    # requests the index, the platform manifest, the config and every layer blob from
    # the mirror, and the pull-through proxy fetches and stores each one it is missing.
    # The layer bytes therefore reach the guest over the loopback forward on first pull
    # instead of crossing the Atlantic from Docker Hub.
    docker pull "127.0.0.1:$PORT/$reference" >/dev/null ||
      fail "Could not warm $reference. Check: docker logs $CONTAINER"
  done
fi

status=0
declare -A observed=()
for reference in "${MIRROR_PATHS[@]}"; do
  digest=$(served_digest "$reference")
  if [[ -z $digest ]]; then
    echo "MISSING  $reference  the mirror resolved no digest" >&2
    status=1
    continue
  fi
  observed[$reference]=$digest
  if [[ $digest == "${EXPECTED[$reference]}" ]]; then
    printf 'OK       %-24s %s\n' "$reference" "$digest"
  elif $update_digests; then
    printf 'UPDATED  %-24s %s (was %s)\n' "$reference" "$digest" "${EXPECTED[$reference]}"
  else
    printf 'MOVED    %-24s %s (pinned %s)\n' "$reference" "$digest" "${EXPECTED[$reference]}" >&2
    status=1
  fi
done

if $update_digests; then
  note 'Adopting the observed digests. They are printed above; apply them deliberately:'
  for reference in "${MIRROR_PATHS[@]}"; do
    echo "  [${reference}]=${observed[$reference]}"
  done
  note 'Edit those three lines in '"$0"' and re-run without --update-digests.'
  exit 0
fi

(( status == 0 )) || fail 'At least one image is missing or its tag has moved. The mirror is running and any images already warmed are still cached; re-run once the cause is understood.'

note 'The mirror is warm and every digest matches the workflow'"'"'s own pull.'
cat <<REPORT

Guest reachability, from this host:
  curl -sS -o /dev/null -w '%{http_code}\n' http://127.0.0.1:$PORT/v2/
  docker image inspect 127.0.0.1:$PORT/library/postgres:18.0 --format '{{index .RepoDigests 0}}'

Guest reachability, inside a running trial guest:
  ip netns exec metal-<vm-id> ssh -i <guest-admin-key> root@172.16.0.2 \\
    'curl -sS -o /dev/null -w "%{http_code}\\n" http://127.0.0.1:$PORT/v2/'
  ip netns exec metal-<vm-id> ssh -i <guest-admin-key> root@172.16.0.2 \\
    'runuser -u runner -- docker info --format "{{json .RegistryConfig.Mirrors}}"'

Then enable it per pool with --registry-mirror-port $PORT on the transport command.
Without that flag nothing in this document changes a single guest.

To remove it, once no pool uses it:
  docker rm -f $CONTAINER && docker volume rm $VOLUME
REPORT