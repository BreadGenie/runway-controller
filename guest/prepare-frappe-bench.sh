#!/usr/bin/env bash
# Bake a working Frappe bench into a fresh, never-registered runner image.
# Run only inside an image-builder guest, after install-guest-runner.sh, and before
# seal-runner-image.sh. prepare-frappe-image.sh calls this once the toolchain it needs is
# in place; it can also be run directly against a throwaway guest to try a change
# without publishing anything. It needs no Docker and no container images, so it does
# not wait for that step and does not care which side of it the build runs.
#
# The dependency profile installs packages; this installs the application. Every job
# otherwise pays to clone the framework, build a virtual environment, install the
# Python and Node dependencies and build the assets. Measured over three disposable
# guests on the published image, Environment Setup averaged 82 s with this bake against
# 191 s for the same guest without it, so this is the work worth about 109 s of a job.
# The in-guest A/B for the bake step alone was 113 s against 23 s. Both are recorded in
# docs/frappe-performance.md; the test phase is untouched by any of it.
#
# The bench is produced by running the same bench commands the workflow runs, not by
# assembling the directories they produce. A hand-built approximation would be a
# different artifact from the one a job expects, and the difference would only show up
# as a job failing later.
set -euo pipefail
set +x

usage() {
  echo 'Usage inside an unregistered builder guest:' >&2
  echo '  sudo bash prepare-frappe-bench.sh --inside-unregistered-image-builder [--frappe-ref REF]' >&2
  exit 2
}
[[ $# -ge 1 && $1 == --inside-unregistered-image-builder ]] || usage
# Consumed here, so the option loop below does not read it as an option it does not know.
shift
frappe_ref=
while (($#)); do
  case $1 in
    --frappe-ref)
      (($# >= 2)) || usage
      frappe_ref=$2
      shift 2
      ;;
    *) usage ;;
  esac
done
[[ $EUID == 0 ]] || { echo 'This bake requires root inside the builder guest.' >&2; exit 1; }
source /etc/os-release
[[ $ID == ubuntu && $VERSION_ID == 24.04 && $(uname -m) == x86_64 ]] || {
  echo 'This image recipe requires Ubuntu 24.04 x86_64.' >&2
  exit 1
}

# Pinned, because an image carrying whatever was on a branch tip would be a different
# artifact on every rebuild and could not be compared with anything. The setup action
# verifies the checkout against this commit and installs normally when it does not
# match, so a job is never silently tested against the wrong source.
readonly frappe_repo=${FRAPPE_REPO:-https://github.com/BreadGenie/frappe.git}
readonly frappe_ref=${frappe_ref:-${FRAPPE_REF:-4423d0a362a702a20f4eee68470ef146081726b0}}

# The job's own workspace, not a template directory. The virtual environment's console
# scripts, and the site configuration, carry absolute paths, so a bench built anywhere
# else has to be rewritten before a job can use it. Building it exactly where the job
# looks removes that whole class of problem, at the cost of the image belonging to one
# repository.
#
# The path is the runner's, not a guess. GitHub Actions sets GITHUB_WORKSPACE to
# $RUNNER_WORKSPACE/<repository name>, and Runway's controller asks for the absolute
# work folder /home/runner/work (controller_engine.go, guestWorkFolder) so that cached
# artifacts are restorable across runner types. That is the default here, and it has to
# be: the alternative, the runner's own relative _work, builds the bench one directory
# away from where a Runway job looks, and a bench is not relocatable. Both directions of
# that mistake were measured on the published image. Built under actions-runner/_work
# and restored into /home/runner/work, the archive extracts cleanly and its virtualenv
# still cannot import frappe, because site-packages/frappe.pth names the build path.
# So a job's fast path would have been reported usable and then failed at Create Site.
#
# RUNNER_WORKSPACE is honoured when set, so a build for a differently laid out runner
# is still possible, but it is not how this image is produced.
readonly runner_workspace=${RUNNER_WORKSPACE:-/home/runner/work}
readonly bench_repository=${FRAPPE_BENCH_REPOSITORY:-frappe}
readonly bench_root=$runner_workspace/$bench_repository/$bench_repository
readonly bench_manifest=/etc/runway-frappe-bench.json
readonly tool_cache=${RUNNER_TOOL_CACHE:-/opt/hostedtoolcache}
readonly python_version=3.14.7
readonly node_version=24.21.0

runner=/home/runner/actions-runner
readonly profile_manifest=/etc/runway-runner-image.txt
[[ -f $profile_manifest && -x $runner/bin/Runner.Listener ]] || {
  echo 'Run install-guest-runner.sh and prepare-frappe-image.sh first; this bake extends that image.' >&2
  exit 1
}
id runner >/dev/null 2>&1 || { echo 'This image has no runner user.' >&2; exit 1; }

# A used runner cannot become a shared image even if its registration files are later
# removed, and a bootstrap marker means a job already ran in this guest.
for credential in .runner .credentials .credentials_rsaparams; do
  [[ -e $runner/$credential ]] && {
    echo 'Refusing to bake into a registered runner. Start from a new guest.' >&2
    exit 1
  }
done
[[ -e /var/lib/runway/bootstrap.json ]] && {
  echo 'Refusing to bake into a guest with a JIT bootstrap marker. Start from a new guest.' >&2
  exit 1
}
# Repeating a bake would have to reconcile with whatever the last one left, and a
# half-reconciled bench is worse than none. Publish the image or start a new guest.
[[ -e $bench_manifest ]] && {
  echo 'This guest already carries a baked bench. Publish that image or start a new guest.' >&2
  exit 1
}
# A bench root without a manifest is worse than no bench root: it is the residue of a
# build that failed partway, and bench init then finds existing apps it cannot account
# for and offers to roll back, which is a question a build script cannot answer.
if [[ -e $bench_root && -n $(find "$bench_root" -mindepth 1 -maxdepth 1 -print -quit) ]]; then
  echo "Refusing to bake into $bench_root, which holds something and no manifest to explain it." >&2
  echo 'Start from a new guest rather than reconciling a partial build.' >&2
  exit 1
fi
# The workspace tmpfs opt-in mounts over this directory and would hide the bake, so a
# guest asked to do both would publish an image whose bench no job can see.
mountpoint -q "$runner_workspace" && {
  echo "Refusing to bake under a mounted $runner_workspace; the bake would be hidden by it." >&2
  exit 1
}
# The base image ships /tmp as ubuntu at 775. The profile step repairs it before its own
# apt, but a bake can be run on its own against a throwaway guest, and then nothing has
# repaired it: apt drops to the _apt account to verify a signature and writes through
# /tmp, so every repository reports "not signed" and the failure names nothing real.
# Repaired here for the same reason as there, and reported because a guest that arrived
# wrong is worth knowing about.
if [[ $(stat -c %a /tmp) != 1777 || $(stat -c %U /tmp) != root ]]; then
  echo "Repairing /tmp, which is $(stat -c '%U:%a' /tmp) rather than root:1777." >&2
  chown root:root /tmp
  chmod 1777 /tmp
fi

readonly python_bin=$tool_cache/Python/$python_version/x64/bin/python3
readonly node_root=$tool_cache/node/$node_version/x64
for required in "$python_bin" "$node_root/bin/node" /usr/local/bin/yarn /usr/local/bin/uv; do
  [[ -x $required ]] || { echo "This image has no $required; run prepare-frappe-image.sh first." >&2; exit 1; }
done

# Everything the job needs and nothing it will replace. The framework checkout, the
# virtual environment, the node modules, the built assets and the bench scaffolding.
# The site's database is deliberately absent: a baked one carries whatever the builder
# did to it, and only a database created by the job can be trusted to start clean.
install -d -m 0755 -o runner -g runner "$bench_root" "$runner_workspace/$bench_repository"
# The work folder itself has to belong to the runner too, and not only the bench below it.
# The runner creates _tool inside it as the runner account, so a root-owned work folder
# fails the job at startup with UnauthorizedAccessException on /home/runner/work/_tool,
# before a single step runs and with nothing in the job log to explain it. Measured on the
# image built from this recipe: the bench directories were runner-owned and the job still
# could not start.
install -d -m 0755 -o runner -g runner "$runner_workspace"

# The build runs as the account the job runs as, because a bench the job cannot write
# is a bench the job cannot use.
run_as_runner() {
  sudo -u runner -- env HOME=/home/runner USER=runner LOGNAME=runner \
    PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
    RUNNER_TOOL_CACHE=$tool_cache "$@"
}

# bench resolves its own directory from the working directory and checks it with
# relative paths, so a command that inherits this process's working directory reads a
# path the runner account cannot open. Over an administrative ssh that directory is
# root's, which is exactly the wrong one, so every bench command is given a directory
# the runner owns instead of inheriting it.
run_as_runner_in() {
  local directory=$1
  shift
  sudo -u runner -- env -C "$directory" HOME=/home/runner USER=runner LOGNAME=runner \
    PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
    RUNNER_TOOL_CACHE=$tool_cache "$@"
}

readonly bench_path=/home/runner/.local/bin:$node_root/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin

echo "Installing the bench tool"
run_as_runner /usr/local/bin/uv tool install frappe-bench
# uv installs tools onto the account's PATH rather than into the Python tool cache, so
# the executable is looked for where uv put it and nowhere else.
readonly bench_bin=/home/runner/.local/bin/bench
[[ -x $bench_bin ]] || { echo "uv did not install a bench at $bench_bin." >&2; exit 1; }

echo "Cloning $frappe_repo at $frappe_ref"
work=/tmp/runway-bench-build
rm -rf "$work"
install -d -m 0755 -o runner -g runner "$work"
run_as_runner git clone --quiet "$frappe_repo" "$work/frappe-src"
run_as_runner git -C "$work/frappe-src" checkout --quiet "$frappe_ref"
# Recorded from the checkout rather than trusted from the argument: the argument is a
# request, the checkout is the fact.
# Read as the runner, not as root: git refuses to operate on a repository owned by
# another account, and root reading a runner-owned checkout is exactly that.
commit=$(run_as_runner git -C "$work/frappe-src" rev-parse HEAD)
expected=$(run_as_runner git -C "$work/frappe-src" rev-parse --verify --quiet "$frappe_ref^{commit}" || echo "$frappe_ref")
[[ $commit == "$expected" ]] || { echo "The checkout is at $commit, not $expected." >&2; exit 1; }

echo 'Initialising the bench at the job workspace path'
run_as_runner_in /home/runner env PATH="$bench_path" \
  "$bench_bin" init "$bench_root" \
    --ignore-exist --frappe-path "$work/frappe-src" --skip-assets --no-backups \
    --python "$python_bin"
# bench init points the app's remote at the checkout it was given, which is a
# build-time staging directory deleted further down. The workflow corrects it for the
# same reason. The remote is named upstream, not origin, so it is repointed by asking
# git rather than by assuming a name.
while read -r remote; do
  run_as_runner git -C "$bench_root/apps/frappe" remote set-url "$remote" "$frappe_repo"
done < <(run_as_runner git -C "$bench_root/apps/frappe" remote)

echo 'Installing dev and test dependencies'
# The system directories stay on PATH: bench shells out to sed, curl and the rest, and
# a PATH carrying only node and the uv tools would fail on the first of them.
run_as_runner_in "$bench_root" env PATH="$bench_path" bash -c '
  set -e
  bench setup requirements --dev
  bench setup requirements --node
'

echo 'Building assets'
run_as_runner_in "$bench_root" env PATH="$bench_path" CI=Yes bash -c '
  set -e
  bench build --force --production
'

# The checkout the bench was initialised from is a build-time staging directory. Leaving
# it would ship a second copy of the framework, and a job checks its own copy out into
# the same workspace.
rm -rf "$work"

echo 'Removing build caches, which no job can use'
# The dependency caches are a by-product of installing, not part of the result: node
# modules and the virtualenv already hold everything a job needs. Shipping them would
# add roughly 1.2 GiB to every copy of this image for nothing. The uv tool directory is
# kept, because that is where the bench executable itself lives.
run_as_runner rm -rf /home/runner/.cache/uv /home/runner/.cache/yarn
run_as_runner mkdir -p /home/runner/.cache

echo 'Verifying the bake before recording it'
# A remote left pointing at the staging directory would send a later fetch somewhere
# that does not exist, and it would only fail on the job that tried it.
if run_as_runner git -C "$bench_root/apps/frappe" remote -v | grep -q -- "$work"; then
  echo "A remote in the bench still points at $work." >&2
  exit 1
fi
# A manifest describing an incomplete bench would let the sealer publish an image whose
# fast path fails on the first job that takes it.
missing=0
# These five are bench's own requirement, taken from paths_in_bench in the tool that is
# about to consume this directory. Checking only the paths this script uses let an archive
# through that bench would not recognise.
for required in apps sites config logs config/pids; do
  if [[ ! -e $bench_root/$required ]]; then
    echo "The bake is missing $required, which bench requires of a bench directory." >&2
    missing=1
  fi
done
for required in \
  "$bench_root/env/bin/python" \
  "$bench_root/apps/frappe/frappe/__init__.py" \
  "$bench_root/apps/frappe/node_modules" \
  "$bench_root/sites/assets/frappe" \
  "$bench_root/sites/common_site_config.json" \
  "$bench_root/Procfile"; do
  if [[ ! -e $required ]]; then
    echo "The bake is missing $required." >&2
    missing=1
  fi
done
[[ $missing == 0 ]] || exit 1
# The commit is read back out of the bench, because the bench's own copy of the app is
# what a job will import. Recording the build directory's commit instead would record a
# fact about a directory that no longer exists.
baked_commit=$(run_as_runner git -C "$bench_root/apps/frappe" rev-parse HEAD)
[[ $baked_commit == "$commit" ]] || {
  echo "The bench carries $baked_commit but the clone was $commit." >&2
  exit 1
}

echo 'Archiving the bench outside the workspace'
# A job's own checkout clears its workspace, so a bench that exists only there is gone by
# the time the job looks for it. This is not a theory: a real job found the workspace
# emptied while the manifest beside the image still claimed a bake. The archive lives
# outside any workspace, so nothing a job does can reach it, and the setup action restores
# from it when the bench is missing.
readonly bench_archive=/opt/runway/bench.tar.gz
install -d -m 0755 -o root -g root /opt/runway
# Log files are left out; the directories they live in are not, because bench refuses to
# run in a directory missing any of apps, sites, config, logs or config/pids. An archive
# that restored into a directory bench would not recognise failed every later command with
# "Command not being executed in bench directory". The pattern is quoted because an
# unquoted *.log is subject to the caller's globbing, which would exclude whatever files
# the build directory happened to contain. The check after this line is what guarantees
# the directories survived.
tar -C "$bench_root" --exclude='*.log' -cf - . | gzip -6 > "$bench_archive"
chown root:root "$bench_archive"
chmod 0644 "$bench_archive"
archive_bytes=$(stat -c %s "$bench_archive")
archive_sha256=$(sha256sum "$bench_archive" | cut -d" " -f1)
printf 'Archived %s bytes of bench to %s\n' "$archive_bytes" "$bench_archive"

# The archive is checked by a script of its own, because the check is about what the tar
# wrote rather than about the directory it was written from, and the only way to test that
# distinction is to call the check directly. It is resolved beside this one, the way the
# sealer resolves its validator.
readonly archive_checker=${BASH_SOURCE[0]%/*}/../scripts/check_bench_archive.sh
[[ -f $archive_checker ]] || {
  echo "check_bench_archive.sh is missing beside the bake at $archive_checker." >&2
  exit 1
}
bash "$archive_checker" "$bench_archive"

echo 'Recording what this image carries'
# Measured from the build that just ran, so the manifest describes the artifact rather
# than the recipe's intentions. The sealer re-checks these against the filesystem before
# anything is destroyed, because the guard deciding what enters a public image must not
# be satisfied by the script that produced its contents.
python3 - "$bench_manifest" "$frappe_repo" "$frappe_ref" "$commit" \
  "$python_version" "$node_version" "$bench_root" "$bench_archive" "$archive_sha256" "$archive_bytes" <<'PYTHON'
import json, os, subprocess, sys

path, repo, ref, commit, python_version, node_version, bench_root = sys.argv[1:8]
archive_path, archive_sha256 = sys.argv[8:10]
# Typed here, not in the shell: an argument arrives as a string, and the sealer
# requires a size, so a manifest written without this fails the seal it is about to
# have to pass.
archive_bytes = int(sys.argv[10])


def size_mib(relative):
    target = os.path.join(bench_root, relative)
    if not os.path.exists(target):
        return 0
    out = subprocess.run(['du', '-sm', target], capture_output=True, text=True, check=True)
    return int(out.stdout.split()[0])


document = {
    'frappe_repository': repo,
    'frappe_ref': ref,
    'frappe_commit': commit,
    'python_version': python_version,
    'node_version': node_version,
    'bench_root': bench_root,
    'archive': {
        'path': archive_path,
        'sha256': archive_sha256,
        'bytes': archive_bytes,
    },
    'size_mib': {
        'app': size_mib('apps/frappe'),
        'env': size_mib('env'),
        'node_modules': size_mib('apps/frappe/node_modules'),
        'assets': size_mib('sites/assets'),
    },
}
handle = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC | os.O_NOFOLLOW, 0o644)
with os.fdopen(handle, 'w') as stream:
    json.dump(document, stream, indent=2, sort_keys=True)
    stream.write('\n')
    stream.flush()
    os.fsync(stream.fileno())
PYTHON

chown -R runner:runner "$bench_root"
echo "Baked $frappe_repo at $commit into $bench_root"