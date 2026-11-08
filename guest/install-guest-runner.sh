#!/usr/bin/env bash
# Execute ONLY inside a fresh disposable Atlas Ubuntu 24.04 x86_64 guest.
# This installs a Docker daemon in the guest, never on the Metal host.
set -euo pipefail
set +x
if [[ $# != 1 || $1 != --inside-disposable-guest ]]; then
  echo 'Usage inside guest only: sudo bash install-guest-runner.sh --inside-disposable-guest' >&2
  exit 2
fi
if [[ $EUID != 0 ]]; then
  echo 'Guest image preparation requires root inside the guest.' >&2
  exit 1
fi
source /etc/os-release
if [[ $ID != ubuntu || $VERSION_ID != 24.04 || $(uname -m) != x86_64 ]]; then
  echo 'This image recipe requires Ubuntu 24.04 x86_64.' >&2
  exit 1
fi

# Stage these reviewed artifacts beside this script before entering the guest.
cache_assets=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
for artifact in cache-node cache-relay runway-cache-relay.service; do
  [[ -f $cache_assets/$artifact && ! -L $cache_assets/$artifact ]] || {
    echo "Missing image input: $cache_assets/$artifact" >&2
    exit 1
  }
done
cache_node_version=$("$cache_assets/cache-node" --runway-cache-version)
[[ $cache_node_version == 'cache-node/v1 allowlist/v1' ]] || {
  echo 'Unsupported cache-node image input.' >&2
  exit 1
}
cache_node_sha256=$(sha256sum "$cache_assets/cache-node" | cut -d ' ' -f1)

runner_version=2.337.0
runner_sha256=70920811a4f8ad4328818682bca5c6469c1c942fab52448868071d0063816613
runner_directory=/home/runner/actions-runner
if [[ -e $runner_directory || -L $runner_directory ]]; then
  echo 'Runner directory already exists. Use a clean guest; do not overwrite registered runner state.' >&2
  exit 1
fi
if id runner >/dev/null 2>&1; then
  if [[ $(id -u runner) != 1001 || $(getent passwd runner | cut -d: -f6) != /home/runner ]]; then
    echo 'Existing runner user must have UID 1001 and home /home/runner.' >&2
    exit 1
  fi
elif getent passwd 1001 >/dev/null; then
  echo 'UID 1001 is already occupied. Use a clean guest.' >&2
  exit 1
else
  useradd --uid 1001 --create-home --shell /bin/bash runner
fi

export DEBIAN_FRONTEND=noninteractive
apt-get update
apt-get install -y --no-install-recommends ca-certificates curl git python3 docker.io
apt-get clean

# Firecracker's 5.10 guest kernel supports legacy netfilter but not nftables.
# Ubuntu defaults to the nft frontend, which prevents Docker bridge creation.
# Probe support before changing the guest's alternatives; never disable NAT.
if ! iptables -t nat -L -n >/dev/null 2>&1; then
  iptables-legacy -t nat -L -n >/dev/null
  ip6tables-legacy -t nat -L -n >/dev/null
  update-alternatives --set iptables /usr/sbin/iptables-legacy
  update-alternatives --set ip6tables /usr/sbin/ip6tables-legacy
fi
if ! iptables -t raw -L -n >/dev/null 2>&1; then
  echo 'Guest kernel lacks the raw netfilter table required by Docker port filtering. Supply a compatible guest kernel; firewall checks are not disabled.' >&2
  exit 1
fi

# A private root-owned staging directory avoids running a dependency installer
# from a directory writable by the unprivileged runner user.
staging_directory=$(mktemp -d /var/tmp/runway-runner.XXXXXX)
trap 'rm -rf -- "$staging_directory"' EXIT
archive="$staging_directory/runner.tar.gz"
curl --fail --location --retry 3 --connect-timeout 20 --max-time 600 \
  "https://github.com/actions/runner/releases/download/v${runner_version}/actions-runner-linux-x64-${runner_version}.tar.gz" \
  --output "$archive"
printf '%s  %s\n' "$runner_sha256" "$archive" | sha256sum --check --status
mkdir "$staging_directory/extracted"
tar -xzf "$archive" --no-same-owner -C "$staging_directory/extracted"
rm -- "$archive"
bash "$staging_directory/extracted/bin/installdependencies.sh"
apt-get clean

usermod --append --groups docker runner
chown -R runner:runner "$staging_directory/extracted"
mv -- "$staging_directory/extracted" "$runner_directory"
# Keep the official Nodes and install the static wrapper at both runner entrypoints.
for version in node20 node24; do
  node_bin=$runner_directory/externals/$version/bin
  mv -- "$node_bin/node" "$node_bin/node.real"
  install -o root -g root -m 0755 "$cache_assets/cache-node" "$node_bin/node"
  chown root:root "$node_bin" "$node_bin/node.real"
  chmod go-w "$node_bin" "$node_bin/node.real"
done
install -d -o root -g root -m 0755 /opt/runway/bin /etc/runway
install -o root -g root -m 0755 "$cache_assets/cache-relay" /opt/runway/bin/cache-relay
install -o root -g root -m 0644 "$cache_assets/runway-cache-relay.service" /etc/systemd/system/runway-cache-relay.service
systemctl daemon-reload
systemctl disable --now runway-cache-relay.service
# No mode, CA or allowlist is baked: prepare configures a fresh clone.
systemctl reset-failed docker
systemctl enable --now docker

{
  printf 'OS=%s\nRUNNER_VERSION=%s\nRUNNER_SHA256=%s\n' "$PRETTY_NAME" "$runner_version" "$runner_sha256"
  printf 'CACHE_NODE_VERSION=%s\nCACHE_NODE_SHA256=%s\n' "$cache_node_version" "$cache_node_sha256"
  dpkg-query --show --showformat='${binary:Package}=${Version}\n' ca-certificates curl git python3 docker.io
} > /etc/runway-runner-image.txt
chmod 0644 /etc/runway-runner-image.txt

# This verifies the runner executable and access to the guest daemon, without
# creating GitHub resources or downloading a Docker image.
runuser -u runner -- "$runner_directory/bin/Runner.Listener" --version
runuser -u runner -- docker version
df -h /
echo 'Guest runner image prepared. Deliver a fresh JIT file separately; do not bake credentials into this image.'
