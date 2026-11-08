#!/usr/bin/env bash
# Run only inside a fresh, never-registered image-builder guest, after installation.
set -euo pipefail
set +x

preserve_container_images=0
inside_builder=0
usage() {
  echo 'Usage: seal-runner-image.sh --inside-unregistered-image-builder [--preserve-container-images]' >&2
}
while (($#)); do
  case $1 in
    --inside-unregistered-image-builder) inside_builder=1; shift ;;
    --preserve-container-images) preserve_container_images=1; shift ;;
    -h | --help) usage; exit 2 ;;
    *) echo "Unknown argument $1." >&2; usage; exit 2 ;;
  esac
done
# Parsing the flag is not the same as requiring it. Sealing decides what a published
# artifact contains, so it runs only when an operator states that this guest is a
# disposable, never-registered builder. Without the flag an operator who mistyped
# could still seal a guest whose provenance nobody asserted.
((inside_builder)) || {
  echo 'Run as root inside an unregistered builder guest with --inside-unregistered-image-builder.' >&2
  usage
  exit 2
}
[[ $EUID == 0 ]] || {
  echo 'Run as root inside an unregistered builder guest with --inside-unregistered-image-builder.' >&2
  exit 2
}

runner=/home/runner/actions-runner
[[ -f /etc/runway-runner-image.txt && -x $runner/bin/Runner.Listener ]] || {
  echo 'The guest runner installer must finish before sealing.' >&2
  exit 1
}
for credential in .runner .credentials .credentials_rsaparams; do
  [[ ! -e $runner/$credential ]] || {
    echo "Refusing to capture a registered runner: $runner/$credential exists. Start from a new guest." >&2
    exit 1
  }
done
[[ -z $(find "$runner" -maxdepth 1 -name '.runner*' -print -quit) ]] || {
  echo 'Refusing to capture a guest with runner registration state. Start from a new guest.' >&2
  exit 1
}

# A JIT bootstrap marker means a job already ran here, whatever the runner
# directory looks like now. It is checked before anything is preserved, because a
# preserved container store must never come from a guest that has run a job.
readonly container_cache_manifest=/etc/runway-container-images.json
readonly jit_bootstrap_marker=/var/lib/runway/bootstrap.json
readonly docker_data_root=/var/lib/docker
# Docker 29 uses the containerd image store, so this is where the images the job
# benefits from actually live. Both directories are preserved, and a store split
# across them is not a store this sealer can vouch for.
readonly containerd_data_root=/var/lib/containerd
# Where the bake records what it built. Read by validate_bench_manifest below, which
# decides whether a claimed bench is really in this guest.
readonly bench_manifest=/etc/runway-frappe-bench.json

# The three service images the Frappe server-integration workflow declares. These
# are repeated here on purpose rather than read out of the manifest: the manifest
# is written by the bake recipe, and the guard that decides whether image content
# survives into a shared artifact must not be satisfied by the same script that
# produced the content. Digests are index digests, so the whole multi-platform
# index is pinned and the amd64 child is recorded for the record.
readonly expected_service_images=(
  'postgres:18.0@sha256:41fc5342eefba6cc2ccda736aaf034bbbb7c3df0fdb81516eba1ba33f360162c'
  'mariadb:11.8@sha256:6422478cb8e159f080fb1d8ccf65101e26fe51385787fde7d16c3b165a331f15'
  'rnwood/smtp4dev:3.7.1@sha256:9356c41b77175aa9dd530a8e1ded29cb2574f2d2474c2b0ab94f202763e6feb9'
)

# Docker records a RepoDigest without the tag (`postgres@sha256:...`), so a pinned
# `postgres:18.0@sha256:...` never appears in that output verbatim.
digest_reference() {
  local reference=$1 name=${1%%@*} repository
  repository=${name%:*}
  printf '%s@%s' "$repository" "${reference##*@}"
}

# The manifest is JSON describing a set of images, so it is read as JSON rather than
# grepped. Every object is required to carry an exact key set: an unexpected key is
# the only way an extra field, including a credential someone added, could ride into
# a public artifact. The image ids are printed for the shell to compare against the
# store, which keeps this guard and the store check independent of each other.
validate_manifest() {
  command -v python3 >/dev/null || {
    echo 'python3 is required to read the container image manifest.' >&2
    echo 'install-guest-runner.sh installs it; do not hand-validate this file with grep.' >&2
    exit 1
  }
  python3 - "$container_cache_manifest" "$docker_data_root" "$containerd_data_root" \
    "${expected_service_images[@]}" <<'python' || exit 1
import json, re, sys

path, data_root, containerd_root, expected = sys.argv[1], sys.argv[2], sys.argv[3], sys.argv[4:]
digest = re.compile(r'sha256:[0-9a-f]{64}$')


def fail(message):
    print(message, file=sys.stderr)
    raise SystemExit(1)


try:
    with open(path) as handle:
        manifest = json.load(handle)
except (OSError, ValueError) as error:
    fail(f'{path} is not readable JSON: {error}')

if not isinstance(manifest, dict):
    fail(f'{path} is not a JSON object.')
top_level = {'schema', 'created_utc', 'data_root', 'containerd_data_root',
             'store_bytes', 'store_bytes_by_root', 'source_registry',
             'image_count', 'images', 'total_bytes'}
if set(manifest) != top_level:
    unexpected = sorted(set(manifest) - top_level)
    missing = sorted(top_level - set(manifest))
    fail(f'{path} key set is wrong; unexpected={unexpected} missing={missing}')
if manifest['schema'] != 'runway.container-images/v1':
    fail(f"{path} declares schema {manifest['schema']!r}.")
if manifest['data_root'] != data_root:
    fail(f"{path} declares data root {manifest['data_root']!r}, not {data_root!r}.")
# Docker 29 keeps image content in containerd, so a manifest that measures only the
# Docker data root understates what this artifact holds by the size of the images.
if manifest['containerd_data_root'] != containerd_root:
    fail(f"{path} declares containerd root {manifest['containerd_data_root']!r}, "
         f"not {containerd_root!r}.")
by_root = manifest['store_bytes_by_root']
if not isinstance(by_root, dict) or set(by_root) != {'docker', 'containerd'}:
    fail(f'{path} store_bytes_by_root has the wrong key set.')
if any(type(value) is not int or value < 0 for value in by_root.values()):
    fail(f'{path} store sizes must be nonnegative integers.')
# store_bytes and total_bytes are the total across both roots, so the roots must sum to
# it. Each root's own value differs from that total, which is the point: under the
# containerd image store the Docker root is a small fraction of the whole.
if manifest['store_bytes'] != sum(by_root.values()):
    fail(f'{path} store_bytes is not the sum of both data roots.')
if manifest['total_bytes'] != manifest['store_bytes']:
    fail(f'{path} total_bytes disagrees with store_bytes.')

images = manifest['images']
if not isinstance(images, list):
    fail(f'{path} does not list images.')
# An exact count, not a subset: a manifest with a fourth entry means the bake ran
# something this guard has never reviewed.
if manifest['image_count'] != len(expected) or len(images) != len(expected):
    fail(f'{path} lists {len(images)} images; this seal preserves {len(expected)}.')
entry_keys = {'reference', 'repository', 'tag', 'amd64_image_id'}
for position, (entry, want) in enumerate(zip(images, expected), start=1):
    if not isinstance(entry, dict) or set(entry) != entry_keys:
        fail(f'{path} image {position} has the wrong key set.')
    if entry['reference'] != want:
        fail(f"Service image {position} is {entry['reference']!r}; this seal only preserves {want!r}.")
    repository, _, tag = want.split('@', 1)[0].rpartition(':')
    if entry['repository'] != repository or entry['tag'] != tag:
        fail(f'{path} image {position} names a repository or tag that does not match its reference.')
    if not digest.fullmatch(str(entry['amd64_image_id'])):
        fail(f'{path} image {position} records no image id for this guest.')
    print(entry['amd64_image_id'])
python
}

assert_no_job_state() {
  [[ ! -e $jit_bootstrap_marker ]] || {
    echo "Refusing to preserve container images: $jit_bootstrap_marker exists, so a job ran in this guest." >&2
    exit 1
  }
  local containers volumes
  containers=$(docker ps -aq)
  [[ -z $containers ]] || {
    echo 'Refusing to preserve container images: this guest has containers. Start from a new guest.' >&2
    docker ps -a >&2
    exit 1
  }
  volumes=$(docker volume ls -q)
  [[ -z $volumes ]] || {
    echo 'Refusing to preserve container images: this guest has volumes, whose contents would be published.' >&2
    docker volume ls >&2
    exit 1
  }
  local dangling
  dangling=$(docker image ls -q --filter dangling=true)
  [[ -z $dangling ]] || {
    echo 'Refusing to preserve container images: dangling images are present, so the store is not only the pinned set.' >&2
    exit 1
  }
  local custom_networks
  custom_networks=$(docker network ls --format '{{.Name}}' | grep -vxE '^(bridge|host|none)$' || true)
  [[ -z $custom_networks ]] || {
    echo "Refusing to preserve container images: custom networks exist: $custom_networks" >&2
    exit 1
  }
}

# A registry credential is how a job authenticates a pull, so it is not image content
# and must never reach a shared artifact. Checked before the daemon is stopped,
# because after the daemon is stopped this can only be checked on the filesystem.
assert_no_docker_auth_config() {
  local config
  for config in /root/.docker "$runner/.docker" /home/runner/.docker \
    /root/.docker/config.json "$runner/.docker/config.json" /home/runner/.docker/config.json; do
    [[ ! -e $config ]] || {
      echo "Refusing to preserve container images: $config exists, so this guest authenticated a registry." >&2
      exit 1
    }
  done
}

assert_container_cache_manifest() {
  [[ -f $container_cache_manifest ]] || {
    echo "--preserve-container-images needs $container_cache_manifest." >&2
    echo 'Run guest/prepare-frappe-container-images.sh in this builder first.' >&2
    exit 1
  }
  # Print the recorded image ids for the comparison in the caller. Validated before
  # anything is stopped: a manifest this seal cannot vouch for must never be
  # followed by a half-preserved store.
  validate_manifest
}

# The bake records what it built. Publication must not take the recipe's word for it,
# for the same reason the container guard above does not: an image whose bench quietly
# disagrees with the workflow is worse than an image with no bench, because every job
# would take the fast path and test code the image never contained. So the recorded
# sizes are compared against the filesystem, rather than trusted as a description of it.
validate_bench_manifest() {
  if [[ ! -f $bench_manifest ]]; then
    echo 'No bench manifest to validate.' >&2
    echo 'Run guest/prepare-frappe-bench.sh in this builder, or do not claim a baked bench.' >&2
    exit 1
  fi
  # The validator is a file of its own so it can be tested without a builder guest.
  # It is resolved relative to this script rather than by absolute path, because the
  # builder carries scripts/ alongside guest/ and the two travel together.
  local scripts_dir
  scripts_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
  local validator=$scripts_dir/validate_bench_manifest.py
  [[ -f $validator ]] || {
    echo 'validate_bench_manifest.py is missing beside the sealer.' >&2
    exit 1
  }
  python3 "$validator" "$bench_manifest" || exit 1
}

# Every pinned image must be present at exactly the reviewed digest, the workflow's
# tag must resolve to that same content, and the store must hold those three images
# and nothing else.
#
# The per-reference id comparison is what binds the store to what a job actually runs.
# Comparing only the set of ids would accept a store where postgres:18.0 resolves to
# mariadb's content and mariadb:11.8 to postgres's: the same three ids, so the set
# matches, and the job then runs the wrong database with no guard noticing.
assert_exact_pinned_store() {
  local reference pinned found tag_id digest_id
  local -a store_image_ids=()
  for reference in "${expected_service_images[@]}"; do
    pinned=$(digest_reference "$reference")
    found=$(docker image inspect --format '{{join .RepoDigests " "}}' "$reference" 2>/dev/null || true)
    [[ " $found " == *" $pinned "* ]] || {
      echo "Preserved store does not contain $pinned." >&2
      echo "Found instead: ${found:-nothing}" >&2
      exit 1
    }
    tag_id=$(docker image inspect --format '{{.Id}}' "${reference%@*}" 2>/dev/null || true)
    digest_id=$(docker image inspect --format '{{.Id}}' "$reference" 2>/dev/null || true)
    [[ -n $tag_id && $tag_id == "$digest_id" ]] || {
      echo "${reference%@*} does not resolve to the content pinned for it." >&2
      echo "Tag id: ${tag_id:-nothing}; pinned id: ${digest_id:-nothing}" >&2
      echo 'A job pulls that tag, so the tag is what has to be right.' >&2
      exit 1
    }
    store_image_ids+=("$tag_id")
  done
  local expected_ids store_ids
  expected_ids=$(printf '%s\n' "${store_image_ids[@]}" | sort -u)
  store_ids=$(docker image ls --no-trunc --format '{{.ID}}' | sort -u)
  [[ $store_ids == "$expected_ids" ]] || {
    echo 'Preserved store holds image content outside the pinned set.' >&2
    printf 'expected:\n%s\nfound:\n%s\n' "$expected_ids" "$store_ids" >&2
    exit 1
  }
  printf '%s\n' "$expected_ids"
}

# A cache-capable image ships helpers but no clone-specific cache configuration.
if grep -qs '^CACHE_NODE_VERSION=' /etc/runway-runner-image.txt; then
  expected_cache_sha=$(awk -F= '$1 == "CACHE_NODE_SHA256" {print $2}' /etc/runway-runner-image.txt)
  [[ $expected_cache_sha =~ ^[0-9a-f]{64}$ ]] || { echo 'Invalid cache-node manifest digest.' >&2; exit 1; }
  for version in node20 node24; do
    node=$runner/externals/$version/bin/node
    [[ -f $node && ! -L $node && -x $node.real ]] || { echo 'Missing baked cache-node or node.real.' >&2; exit 1; }
    printf '%s  %s\n' "$expected_cache_sha" "$node" | sha256sum --check --status
    [[ $("$node" --runway-cache-version) == 'cache-node/v1 allowlist/v1' ]] || exit 1
  done
  [[ -x /opt/runway/bin/cache-relay && -f /etc/systemd/system/runway-cache-relay.service ]] || {
    echo 'Missing baked cache relay or unit.' >&2; exit 1;
  }
fi

# Docker client configuration is removed on both paths, alongside the SSH host keys
# below: anything a job or the bake wrote there is not image content. On the
# preserving path the removal happens only after the guard above has checked that
# there was nothing there to begin with, so the guard sees the guest as it was.
# A claimed bench is checked before anything is preserved or stopped, and on both seal
# paths, because a Frappe image can carry a bench without carrying container images.
# It runs here so a guest this seal cannot vouch for is refused while it is still
# inspectable, rather than half-preserved on the way to finding out.
if grep -qs '^FRAPPE_BENCH_BAKED=yes$' /etc/runway-runner-image.txt; then
  validate_bench_manifest
elif [[ -f $bench_manifest ]]; then
  echo 'This guest carries a bench manifest but its image manifest does not claim one.' >&2
  echo 'Publish it as a Frappe bake or remove the manifest; do not seal it as a plain profile.' >&2
  exit 1
fi

if ((preserve_container_images)); then
  [[ -d $docker_data_root ]] || {
    echo "--preserve-container-images found no $docker_data_root in this guest." >&2
    echo 'Run guest/prepare-frappe-container-images.sh in this builder first.' >&2
    exit 1
  }
  systemctl start docker.service
  # Validate the guest, the manifest, the exact image set and the absence of any
  # registry credential before the daemon is stopped. Nothing is destroyed first:
  # a refusal here still leaves a guest an operator can inspect.
  assert_no_job_state
  assert_no_docker_auth_config
  # Both id lists are sorted so the comparison below is about content, not about the
  # order the manifest happens to list images in.
  manifest_image_ids=$(assert_container_cache_manifest | sort -u)
  # Build cache is job scratch that the bake never needed. Drop it so the
  # preserved store is image content and nothing else.
  docker builder prune --all --force >/dev/null
  store_image_ids=$(assert_exact_pinned_store | sort -u)
  # The manifest and the store must agree on which content this is, so neither one
  # alone decides what is published.
  if [[ $manifest_image_ids != "$store_image_ids" ]]; then
    echo "$container_cache_manifest records different image content than the store holds." >&2
    printf 'manifest:\n%s\nstore:\n%s\n' "$manifest_image_ids" "$store_image_ids" >&2
    exit 1
  fi
  # Prove the store is usable after the wipe of client config and before it is
  # frozen, rather than discovering it broken in the first job that needs it.
  docker image inspect --format '{{.Id}}' "${expected_service_images[0]%@*}" >/dev/null
  systemctl stop docker.service docker.socket
  # The store is only consistent once nothing is writing to it. containerd holds
  # image content when the daemon uses the containerd image store, so it is part of
  # what is preserved and it is stopped with docker rather than left running.
  systemctl stop containerd.service
  for unit in docker.service docker.socket containerd.service; do
    if systemctl is-active --quiet "$unit"; then
      echo "$unit is still active; refusing to freeze a container store that is being written to." >&2
      exit 1
    fi
  done
  echo 'Preserving a validated container image store. Only image content survives; containers, volumes and client config do not.'
else
  systemctl stop docker.service docker.socket containerd.service atlas-metadata.service
  # The clean image has no jobs or containers. Each boot gets fresh daemon state.
  rm -rf "$docker_data_root" /var/lib/containerd
fi

if grep -qs '^CACHE_NODE_VERSION=' /etc/runway-runner-image.txt; then
  systemctl disable --now runway-cache-relay.service
  rm -f /etc/runway/cache.json /etc/runway/cache-ca.pem /etc/runway/cache-allowlist.json
  rm -rf /run/runway
fi
systemctl stop atlas-metadata.service
# sshd has to be stopped before the host keys go, not merely after. sshd regenerates a
# missing host key when it starts, so deleting the files while it is still running leaves
# a guest that grows a fresh identity minutes later: an image built this way sealed
# cleanly and still exported a host key, and the finalizer refused it. Stopping the
# service first is what makes the deletion the last word. The guest is disposable and is
# destroyed after export, so nothing here is needed afterwards.
systemctl stop ssh.service ssh.socket sshd.service 2>/dev/null || true
rm -rf "$runner/_diag" "$runner/_work"
apt-get clean
rm -rf /var/lib/apt/lists/*
cloud-init clean --logs --machine-id
find /etc/ssh -maxdepth 1 -type f -name 'ssh_host_*' -delete
for home in /root /home/runner /home/ubuntu; do
  [[ -d $home ]] || continue
  rm -rf "$home/.ssh" "$home/.cache" "$home/.docker"
  rm -f "$home/.bash_history" "$home/.python_history" "$home/.git-credentials"
done
rm -f /var/lib/systemd/random-seed /var/lib/dbus/machine-id
ln -s /etc/machine-id /var/lib/dbus/machine-id
printf 'runway-runner\n' > /etc/hostname
printf '127.0.0.1 localhost\n::1 localhost ip6-localhost ip6-loopback\n' > /etc/hosts
find /var/log -type f -exec truncate -s 0 {} +
find /tmp /var/tmp -mindepth 1 -maxdepth 1 -exec rm -rf {} +
sync
if ((preserve_container_images)); then
  printf 'Sealed unregistered image with pre-pulled service images; stop this VM, capture, then destroy it. Do not register or reuse it.\n'
else
  printf 'Sealed unregistered image; stop this VM, capture, then destroy it. Do not register or reuse it.\n'
fi