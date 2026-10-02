#!/usr/bin/env bash
# Finalize an exported, sealed, unmounted builder snapshot. Never pass a live disk.
set -euo pipefail
[[ $# == 1 && -f $1 && ! -L $1 ]] || { echo 'Usage: finalize-runner-image.sh EXPORTED_EXT4_FILE' >&2; exit 2; }
image=$(realpath "$1")
export PATH="$PATH:/usr/sbin:/sbin"
for tool in e2fsck resize2fs tune2fs zerofree debugfs zstd; do
  command -v "$tool" >/dev/null || { echo "Missing $tool" >&2; exit 1; }
done
check_filesystem() {
  local status=0
  e2fsck -f -p "$image" || status=$?
  [[ $status -le 1 ]] || return "$status"
}
# Repair the filesystem before inspecting it. A stopped VM's export carries a
# dirty ext4 journal, and debugfs reads a filesystem without replaying one, so a
# present file can be reported as missing and an inode bitmap can fail its
# checksum for reasons no credential problem explains.
#
# e2fsck -p never answers yes on the user's behalf and never makes a change it is
# unsure about, so a status above 1 means the export needs a person. Stop here
# rather than inspecting it, and never relax the checks below: they are the only
# credential guard a published image gets.
if ! check_filesystem; then
  echo "e2fsck could not repair $image unattended." >&2
  echo 'Review this unmounted export yourself, repeat the finalizer on a clean result, and verify a fresh VM afterwards.' >&2
  exit 1
fi
# The sealing marker and absent registration files are required, not a substitute
# for creating a fresh builder that never received JIT credentials. An unreadable
# filesystem and a missing marker look identical from a bare exit status, so say
# which one stopped the export rather than leaving the operator to guess.
if ! debugfs -R 'stat /etc/runway-runner-image.txt' "$image" 2>/dev/null | grep -q '^Inode:'; then
  echo "$image has no /etc/runway-runner-image.txt." >&2
  echo 'That file is the package manifest the guest installer writes, so this export is not a sealed builder image.' >&2
  echo 'Do not publish it. Seal a fresh, never-registered builder and export it again.' >&2
  exit 1
fi
for path in /home/runner/actions-runner/.runner /home/runner/actions-runner/.credentials /home/runner/actions-runner/.credentials_rsaparams /etc/ssh/ssh_host_ed25519_key /etc/ssh/ssh_host_rsa_key; do
  if debugfs -R "stat $path" "$image" 2>/dev/null | grep -q '^Inode:'; then
    echo "Refusing unsealed image containing $path" >&2; exit 1
  fi
done
check_filesystem
# Metal's base volume adds 64 MiB slack. Resize the filesystem before truncating.
resize2fs "$image" 4G
truncate -s 4294967296 "$image"
# Old journal blocks can retain deleted identity material. Replace the journal
# and zero unallocated blocks before publishing a shared System image.
tune2fs -O ^has_journal "$image"
check_filesystem
zerofree "$image"
tune2fs -j "$image"
check_filesystem
zstd -T2 -f -q "$image" -o "$image.zst"
sha256sum "$image" "$image.zst"
