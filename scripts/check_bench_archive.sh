#!/usr/bin/env bash
# Assert that a bench archive holds the directories bench requires of a bench directory.
# Used by guest/prepare-frappe-bench.sh before it records what it baked.
set -euo pipefail
set +x

if [[ $# -ne 1 ]]; then
  echo 'Usage: check_bench_archive.sh ARCHIVE' >&2
  exit 2
fi

archive=$1
if [[ ! -f $archive ]]; then
  echo "No archive to check at $archive." >&2
  exit 1
fi

# bench refuses to run in a directory missing any of these five, taken from
# paths_in_bench in the tool that is about to consume the directory. An archive that
# restored into a directory bench would not recognise fails every later command with
# "Command not being executed in bench directory".
#
# The check is made against the archive's own listing rather than against the directory
# it was made from. That distinction is the whole reason this is a separate script: a
# tar exclusion drops a directory from the archive while leaving it present in the build
# tree, so a check of the tree passes and publishes an image whose bench cannot run.
listing=$(tar tzf "$archive")
missing=0
for required in apps sites config logs config/pids; do
  if ! grep -qx "\./$required" <<<"$listing" && ! grep -q "^\./$required/" <<<"$listing"; then
    echo "The archive has no $required, which bench requires of a bench directory." >&2
    missing=1
  fi
done
[[ $missing == 0 ]] || exit 1
