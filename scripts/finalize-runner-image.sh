#!/usr/bin/env bash
# Finalize an exported, sealed, unmounted builder snapshot. Never pass a live disk.
set -euo pipefail
set +x

# The published size is a truncation target, not a capacity plan. A profile that
# carries pre-pulled service container images needs more than the 4 GiB default, so
# the size is an explicit operator choice rather than a constant buried in the
# script. Defaults, bounds and back-compat: no flag keeps the previous behaviour
# exactly, and the flag may appear before or after the image path. The upper bound
# is 16 GiB rather than an open range: an image large enough to hold the baked
# service images is about 7 GiB, and anything past 16 is a capacity mistake rather
# than a profile, so the finalizer refuses it instead of publishing a pool-sized
# artifact under a runner-image name.
readonly default_size_gib=4
readonly min_size_gib=4
readonly max_size_gib=16
size_gib=$default_size_gib
usage() {
  echo "Usage: finalize-runner-image.sh [--size-gib ${min_size_gib}-${max_size_gib}] EXPORTED_EXT4_FILE" >&2
}
image=
while (($#)); do
  case $1 in
    --size-gib)
      (($# >= 2)) || { usage; echo '--size-gib needs a value.' >&2; exit 2; }
      size_gib=$2
      shift 2
      ;;
    --size-gib=*)
      size_gib=${1#--size-gib=}
      shift
      ;;
    -h | --help)
      usage
      exit 2
      ;;
    --)
      shift
      break
      ;;
    -*)
      echo "Unknown option $1." >&2
      usage
      exit 2
      ;;
    *)
      [[ -z $image ]] || { echo 'Pass exactly one exported filesystem.' >&2; usage; exit 2; }
      image=$1
      shift
      ;;
  esac
done
while (($#)); do
  [[ -z $image ]] || { echo 'Pass exactly one exported filesystem.' >&2; usage; exit 2; }
  image=$1
  shift
done

# Validate the requested size before touching the export. An unparseable or
# out-of-range value has to fail as a usage error, before the minutes-long e2fsck
# pass, or an operator finds out about a typo only after a repair has already run.
[[ ${#size_gib} -le 2 && $size_gib =~ ^[0-9]+$ ]] || { echo "--size-gib must be a whole number, got '$size_gib'." >&2; exit 2; }
# A leading zero is a whole number to bash but not to resize2fs, and 4G is not 04G
# to a reader either. Reject it so the value in the log matches what was applied.
[[ $size_gib == "${size_gib#"${size_gib%%[!0]*}"}" ]] || { echo "--size-gib must not have leading zeros, got '$size_gib'." >&2; exit 2; }
if ((size_gib < min_size_gib || size_gib > max_size_gib)); then
  echo "--size-gib must be between $min_size_gib and $max_size_gib, got $size_gib." >&2
  echo "A published image below ${min_size_gib} GiB cannot hold the dependency profile this image recipe builds." >&2
  exit 2
fi

[[ -n $image && -f $image && ! -L $image ]] || { usage; exit 2; }
image=$(realpath "$image")
readonly size_bytes=$((size_gib * 1024 * 1024 * 1024))

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
#
# There is deliberately no size comparison before the resize. `debugfs stats` reports
# the filesystem's total capacity, not the content in it, so a 10 GiB builder measured
# that way looks too large for every published size and a shrink would always be
# refused. resize2fs is the authority on whether content fits: it fails when it does
# not, and `set -e` stops the export before anything is truncated.
resize2fs "$image" "${size_gib}G"
# Now the filesystem is the target size, confirm the resize produced that before
# truncating. Capacity is what this measures, which is correct here because the
# filesystem has already been resized: exceeding the target means resize2fs did not do
# what was asked, and cutting the file shorter than its own filesystem would discard
# content.
final_blocks=$(debugfs -R 'stats' "$image" 2>/dev/null | awk -F: '/^Block count/{gsub(/ /,"",$2); print $2; exit}')
final_block_size=$(debugfs -R 'stats' "$image" 2>/dev/null | awk -F: '/^Block size/{gsub(/ /,"",$2); print $2; exit}')
[[ $final_blocks =~ ^[0-9]+$ && $final_block_size =~ ^[0-9]+$ ]] || {
  echo "Could not read the filesystem size of $image from debugfs after resizing." >&2
  echo 'Refusing to truncate an export whose size is unknown.' >&2
  exit 1
}
final_bytes=$((final_blocks * final_block_size))
if ((final_bytes > size_bytes)); then
  echo "After resizing to ${size_gib} GiB the filesystem reports $final_bytes bytes." >&2
  echo "Truncating it to $size_bytes bytes would discard content. Not publishing this export." >&2
  exit 1
fi
truncate -s "$size_bytes" "$image"
# Old journal blocks can retain deleted identity material. Replace the journal
# and zero unallocated blocks before publishing a shared System image.
tune2fs -O ^has_journal "$image"
check_filesystem
zerofree "$image"
tune2fs -j "$image"
check_filesystem
zstd -T2 -f -q "$image" -o "$image.zst"
sha256sum "$image" "$image.zst"
echo "Published filesystem target: ${size_gib} GiB ($size_bytes bytes)."