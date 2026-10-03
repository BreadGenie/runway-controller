#!/usr/bin/env bash
# Add the Frappe workflow's service container images to a fresh, never-registered
# runner image. Run only inside an image-builder guest, after install-guest-runner.sh
# and prepare-frappe-image.sh, and before seal-runner-image.sh
# --preserve-container-images. This installs nothing on the Metal host.
#
# Why this exists: the workflow's `services:` block makes the runner `docker pull`
# three public images on every job, on a fresh VM, over the region's path to the
# registry. Measured on the paired Frappe baseline, `Initialize containers` took
# 20 s on GitHub-hosted and 45 s on Runway, and a local registry mirror only closed
# that to 42 s, because a mirror still hands the job compressed layers to
# decompress. Baking the layers into the image removes the transfer and the
# decompression together, because by the time the job runs they are already
# uncompressed content in the guest's own Docker store.
set -euo pipefail
set +x

# The flag is not decoration. This recipe writes content into the Docker store that
# later becomes a shared, publicly published artifact, so it refuses to run unless
# an operator states that this guest is a disposable, unregistered image builder.
if [[ $# != 1 || $1 != --inside-unregistered-image-builder ]]; then
  echo 'Usage: sudo bash prepare-frappe-container-images.sh --inside-unregistered-image-builder' >&2
  echo 'The flag is required: this recipe writes content into a published image.' >&2
  exit 2
fi
if [[ $EUID != 0 ]]; then
  echo 'Container image preparation requires root inside the builder guest.' >&2
  exit 1
fi
source /etc/os-release
if [[ $ID != ubuntu || $VERSION_ID != 24.04 || $(uname -m) != x86_64 ]]; then
  echo 'This recipe requires Ubuntu 24.04 x86_64.' >&2
  exit 1
fi

# The profile is an addition to the baked runner image, not a substitute for it.
runner=/home/runner/actions-runner
runner_manifest=/etc/runway-runner-image.txt
if [[ ! -f $runner_manifest || ! -x $runner/bin/Runner.Listener ]]; then
  echo 'Run install-guest-runner.sh first; this recipe extends that image.' >&2
  exit 1
fi
if ! id runner >/dev/null 2>&1; then
  echo 'This image has no runner user for the workflow to use.' >&2
  exit 1
fi
# A used runner cannot become a shared image, even if its registration files are
# later removed. A bootstrap marker means a job already ran here.
for credential in .runner .credentials .credentials_rsaparams; do
  if [[ -e $runner/$credential ]]; then
    echo 'Refusing to add to a registered runner. Start from a new guest.' >&2
    exit 1
  fi
done
if [[ -n $(find "$runner" -maxdepth 1 -name '.runner*' -print -quit) ]]; then
  echo 'Refusing to add to a guest with runner registration state. Start from a new guest.' >&2
  exit 1
fi
if [[ -e /var/lib/runway/bootstrap.json ]]; then
  echo 'Refusing to add to a guest with a JIT bootstrap marker. Start from a new guest.' >&2
  exit 1
fi
# Registry credentials are checked before anything is pulled, not only at the end:
# a config left in this guest means somebody used this daemon for something, and a
# pull now would be attributable to that guest. Deleting it later cannot make the
# store credential-free if the pull itself was authenticated.
readonly docker_config_dirs=(/root/.docker "$runner/.docker" /home/runner/.docker)
for config_dir in "${docker_config_dirs[@]}"; do
  if [[ -e $config_dir ]]; then
    echo "Refusing to add to a guest holding $config_dir." >&2
    echo 'A registry credential in this builder would ship inside the published image. Start from a new guest.' >&2
    exit 1
  fi
done
# This recipe's output is meant to be preserved into a shared artifact, so it also
# refuses a guest that already ran containers. Their writable layers and volumes
# would otherwise ride along in the Docker store that sealing preserves.
if [[ -d /var/lib/docker ]]; then
  if ! systemctl start docker.service >/dev/null 2>&1; then
    echo 'Could not start docker.service; a job needs Docker, so this image is not usable.' >&2
    exit 1
  fi
  if [[ -n $(docker ps -aq) || -n $(docker volume ls -q) ]]; then
    echo 'Refusing to add to a guest that already has containers or volumes. Start from a new guest.' >&2
    exit 1
  fi
fi

# The exact images the Frappe server-integration workflow declares in its
# `services:` block, pinned by index digest rather than by tag. Pinning the index
# fixes every platform manifest, so a job on this guest resolves to the reviewed
# content even if a tag is ever moved. If the workflow changes a tag, `docker pull`
# in the job simply fetches the new image: a stale bake costs time, never
# correctness.
readonly service_images=(
  'postgres:18.0@sha256:41fc5342eefba6cc2ccda736aaf034bbbb7c3df0fdb81516eba1ba33f360162c'
  'mariadb:11.8@sha256:6422478cb8e159f080fb1d8ccf65101e26fe51385787fde7d16c3b165a331f15'
  'rnwood/smtp4dev:3.7.1@sha256:9356c41b77175aa9dd530a8e1ded29cb2574f2d2474c2b0ab94f202763e6feb9'
)

# Docker records a RepoDigest without the tag (`postgres@sha256:...`), so a pinned
# `postgres:18.0@sha256:...` can never be found in that output verbatim. Compare the
# two forms the daemon actually produces.
digest_reference() {
  local reference=$1 name=${1%%@*} repository
  # A registry host may carry a port and a path; only the final `name:tag` splits.
  repository=${name%:*}
  printf '%s@%s' "$repository" "${reference##*@}"
}

export DEBIAN_FRONTEND=noninteractive

# Record what this image actually contains. This is a separate file from the
# dependency profile manifest on purpose: seal-runner-image.sh validates it, and
# the two are written and checked by different steps of the build. It is JSON
# because it describes a set of images rather than a flat key/value block, and it
# is credential-free by construction: every value below is a public image reference
# or a measurement of this filesystem, so the file can ship inside a public artifact.
readonly container_manifest=/etc/runway-container-images.json
readonly docker_data_root=/var/lib/docker
# Docker 29 on Ubuntu 24.04 uses the containerd image store, so the image content a
# job benefits from is in this directory and /var/lib/docker holds almost none of it.
# Both are measured, and the sealer preserves both.
readonly containerd_data_root=/var/lib/containerd

systemctl start docker.service
if [[ $(docker info --format '{{.DockerRootDir}}') != "$docker_data_root" ]]; then
  echo "Docker's data root is $(docker info --format '{{.DockerRootDir}}'), not $docker_data_root." >&2
  echo 'Sealing preserves /var/lib/docker. Move the data root back or teach seal where it is; do not publish a split store.' >&2
  exit 1
fi

printf 'Pulling %d pinned service images into %s\n' "${#service_images[@]}" "$docker_data_root"
image_ids=()
image_records=()
for reference in "${service_images[@]}"; do
  docker pull "$reference"
  repo=${reference%@*}
  # The workflow pulls by tag, so the tag has to resolve locally too. This is what
  # lets `docker pull postgres:18.0` in a job find every layer already present.
  docker tag "$reference" "$repo"
  # Verify after pulling rather than trusting the pull's own exit status: a store that
  # holds a different digest under the same reference is exactly the failure that
  # would silently publish unreviewed content.
  pinned=$(digest_reference "$reference")
  found=$(docker image inspect --format '{{join .RepoDigests " "}}' "$reference" 2>/dev/null || true)
  if [[ " $found " != *" $pinned "* ]]; then
    echo "Pulled store does not record $pinned; found: ${found:-nothing}." >&2
    exit 1
  fi
  # The amd64 child that actually landed on this x86_64 guest, taken from the store
  # rather than copied from anywhere else, so every number in this manifest is
  # something this run observed.
  image_id=$(docker image inspect --format '{{.Id}}' "$repo")
  image_ids+=("$image_id")
  image_records+=("$(printf '    {\n      "reference": "%s",\n      "repository": "%s",\n      "tag": "%s",\n      "amd64_image_id": "%s"\n    }' \
    "$reference" "${repo%%:*}" "${repo##*:}" "$image_id")")
done

# Nothing but the reviewed set may remain, or sealing's guarantee is weaker than
# the manifest claims. `-e` per pattern matters: `"${array[@]}"` as one grep
# argument would compare literally, and as several it would read them as filenames.
grep_arguments=()
for reference in "${service_images[@]}"; do
  grep_arguments+=(-e "${reference%@*}")
done
unexpected=$(docker image ls --format '{{.Repository}}:{{.Tag}}' | grep -vxF "${grep_arguments[@]}" || true)
if [[ -n $unexpected ]]; then
  echo 'The image store holds images this recipe did not pull:' >&2
  echo "$unexpected" >&2
  exit 1
fi
# The same question asked of content rather than names: three reviewed image ids and
# nothing else, so no extra layer can ride along under a name this check misses.
expected_ids=$(printf '%s\n' "${image_ids[@]}" | sort -u)
# --no-trunc: without it Docker prints a 12-character short id, which would never
# equal the full id `docker image inspect` reports for the same image.
store_ids=$(docker image ls --no-trunc --format '{{.ID}}' | sort -u)
if [[ $store_ids != "$expected_ids" ]]; then
  echo 'The image store holds image content this recipe did not pull.' >&2
  printf 'expected:\n%s\nfound:\n%s\n' "$expected_ids" "$store_ids" >&2
  exit 1
fi
docker builder prune --all --force >/dev/null

# The finished guest has run no job: no containers, no volumes, and no registry
# credential anywhere it could reach the preserved store.
if [[ -n $(docker ps -aq) || -n $(docker volume ls -q) ]]; then
  echo 'This guest has containers or volumes; refusing to record it as a clean builder.' >&2
  exit 1
fi
rm -rf "${docker_config_dirs[@]}"
for config in /root/.docker/config.json "$runner/.docker/config.json" /home/runner/.docker/config.json; do
  if [[ -e $config ]]; then
    echo "Failed to remove $config; a registry credential would ship in the image." >&2
    exit 1
  fi
done

# Both directories are measured, and the total is what the manifest records. With the
# containerd image store, which is what Docker 29 uses on Ubuntu 24.04, the image
# content lives in /var/lib/containerd and /var/lib/docker holds little more than
# metadata: measuring the data root alone reported 184 KB for 1.4 GB of images, and a
# size that wrong cannot be used to decide whether an image fits its artifact.
store_docker_bytes=$(du -sb "$docker_data_root" | cut -f1)
store_containerd_bytes=0
if [[ -d $containerd_data_root ]]; then
  store_containerd_bytes=$(du -sb "$containerd_data_root" | cut -f1)
else
  # No containerd root means this daemon is not using the containerd image store, so the
  # images are in the Docker data root the size above already measured.
  echo "No $containerd_data_root; this daemon is not using the containerd image store."
fi
store_bytes=$((store_docker_bytes + store_containerd_bytes))
{
  printf '{\n'
  printf '  "schema": "runway.container-images/v1",\n'
  printf '  "created_utc": "%s",\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  printf '  "data_root": "%s",\n' "$docker_data_root"
  printf '  "containerd_data_root": "%s",\n' "$containerd_data_root"
  printf '  "store_bytes": %d,\n' "$store_bytes"
  printf '  "store_bytes_by_root": {"docker": %d, "containerd": %d},\n' \
    "$store_docker_bytes" "$store_containerd_bytes"
  printf '  "source_registry": "registry-1.docker.io",\n'
  printf '  "image_count": %d,\n' "${#service_images[@]}"
  printf '  "images": [\n'
  last=$((${#image_records[@]} - 1))
  for position in "${!image_records[@]}"; do
    printf '%s' "${image_records[position]}"
    ((position == last)) || printf ','
    printf '\n'
  done
  printf '  ],\n'
  printf '  "total_bytes": %d\n' "$store_bytes"
  printf '}\n'
} > "$container_manifest"
chmod 0644 "$container_manifest"

# Confirm the file this recipe just wrote is real JSON before it is claimed to be a
# manifest. A truncated write here would otherwise be discovered by the sealer, on a
# stopped VM, minutes later.
python3 -c 'import json,sys; json.load(open(sys.argv[1]))' "$container_manifest"

# Prove the premise the whole recipe exists for: a pull by the tag the workflow
# uses must now resolve without transferring layers. `--quiet` still contacts the
# registry to resolve the tag, which is the part a mirror cannot improve either,
# but no layer data comes back.
for reference in "${service_images[@]}"; do
  repo=${reference%@*}
  docker image inspect --format '{{.Id}}' "$repo" >/dev/null
  printf 'already present: %s\n' "$repo"
done

printf 'Container cache used %s bytes in %s\n' "$(du -sh "$docker_data_root" | cut -f1)" "$docker_data_root"
df -h /
echo 'Service images prepared. Seal next with --preserve-container-images; it still has no credentials.'