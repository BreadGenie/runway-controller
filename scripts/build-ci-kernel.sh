#!/usr/bin/env bash
# Build an initramfs-free Firecracker kernel with Docker bridge networking.
set -euo pipefail
build_dir=${1:?Usage: build-ci-kernel.sh BUILD_DIRECTORY}
mkdir -p "$build_dir"
build_dir=$(realpath "$build_dir")
source_commit=8c3eeb0cc41e3fa4e5e27e29c557124ed1a1c451
config_commit=791763f545dd73c1f5f22c67dd314009ad8047d9
source_dir="$build_dir/linux-$source_commit"
if [[ ! -d $source_dir ]]; then
  curl -fL "https://codeload.github.com/amazonlinux/linux/tar.gz/$source_commit" -o "$build_dir/source.tar.gz"
  tar -xzf "$build_dir/source.tar.gz" -C "$build_dir"
fi
curl -fsSL "https://raw.githubusercontent.com/firecracker-microvm/firecracker/$config_commit/resources/guest_configs/microvm-kernel-ci-x86_64-5.10.config" -o "$build_dir/firecracker-base.config"
cd "$source_dir"
cp "$build_dir/firecracker-base.config" .config
for option in IP_NF_RAW IP6_NF_RAW NETFILTER_XT_TARGET_CT NETFILTER_XT_MATCH_COMMENT NETFILTER_XT_MATCH_MULTIPORT NETFILTER_XT_MATCH_PHYSDEV NETFILTER_XT_MATCH_MARK NETFILTER_XT_MARK NETFILTER_XT_CONNMARK NETFILTER_XT_MATCH_STATE NF_TABLES NF_TABLES_INET NF_TABLES_IPV4 NF_TABLES_IPV6 NFT_CT NFT_COUNTER NFT_LOG NFT_LIMIT NFT_MASQ NFT_REDIR NFT_NAT NFT_REJECT NFT_COMPAT NFT_CHAIN_NAT IKCONFIG IKCONFIG_PROC; do
  scripts/config --enable "$option"
done
scripts/config --set-str LOCALVERSION '-fc-ci'
make HOSTCC="gcc -std=gnu11" olddefconfig
for option in IP_NF_RAW IP6_NF_RAW NETFILTER_XT_TARGET_CT VIRTIO_MMIO VIRTIO_BLK VIRTIO_NET EXT4_FS OVERLAY_FS BRIDGE BRIDGE_NETFILTER; do
  grep -qx "CONFIG_${option}=y" .config || { echo "Missing built-in $option" >&2; exit 1; }
done
make -j3 CC="gcc -std=gnu11" HOSTCC="gcc -std=gnu11" vmlinux
cp vmlinux "$build_dir/vmlinux-5.10.223-fc-ci"
cp .config "$build_dir/config-5.10.223-fc-ci"
sha256sum "$build_dir/vmlinux-5.10.223-fc-ci" "$build_dir/config-5.10.223-fc-ci" > "$build_dir/SHA256SUMS"
printf 'kernel_source_commit=%s\nfirecracker_config_commit=%s\n' "$source_commit" "$config_commit" > "$build_dir/provenance.txt"
