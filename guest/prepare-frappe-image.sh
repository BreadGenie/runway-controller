#!/usr/bin/env bash
# Add the Frappe dependency profile to a fresh, never-registered runner image.
# Run only inside an image-builder guest, after install-guest-runner.sh and
# before seal-runner-image.sh. This installs nothing on the Metal host.
set -euo pipefail
set +x

if [[ $# != 1 || $1 != --inside-unregistered-image-builder ]]; then
  echo 'Usage inside an unregistered builder guest: sudo bash prepare-frappe-image.sh --inside-unregistered-image-builder' >&2
  exit 2
fi
if [[ $EUID != 0 ]]; then
  echo 'Frappe image preparation requires root inside the builder guest.' >&2
  exit 1
fi
source /etc/os-release
if [[ $ID != ubuntu || $VERSION_ID != 24.04 || $(uname -m) != x86_64 ]]; then
  echo 'This image recipe requires Ubuntu 24.04 x86_64.' >&2
  exit 1
fi

# The profile is an addition to the baked runner image, not a substitute for it.
runner=/home/runner/actions-runner
manifest=/etc/runway-runner-image.txt
if [[ ! -f $manifest || ! -x $runner/bin/Runner.Listener ]]; then
  echo 'Run install-guest-runner.sh first; this profile extends that image.' >&2
  exit 1
fi
if ! id runner >/dev/null 2>&1; then
  echo 'This image has no runner user for the setup actions to use.' >&2
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

# The package set mirrors .github/actions/setup in the Frappe workflow repository,
# so a job finds its dependencies already installed. It stays a dependency set:
# Frappe itself, bench, Python wheels, and node modules are the job's work.
readonly mariadb_server_version=mariadb-11.8.5
readonly mariadb_repo_setup_url=https://r.mariadb.com/downloads/mariadb_repo_setup
readonly postgres_keyring=/usr/share/keyrings/pgdg.gpg
readonly postgres_key_url=https://www.postgresql.org/media/keys/ACCC4CF8.asc
readonly postgres_suite=${VERSION_CODENAME}-pgdg
readonly wkhtmltox_version=0.12.6.1-2
readonly wkhtmltox_deb_name=wkhtmltox_0.12.6.1-2.jammy_amd64.deb
readonly wkhtmltox_url=https://github.com/wkhtmltopdf/packaging/releases/download/${wkhtmltox_version}/${wkhtmltox_deb_name}
# The jammy build is the patched Qt release the Frappe PDF path needs. The
# workflow caches this package at /tmp/wkhtmltox.deb and sealing empties /tmp, so
# a clone's first job fetches it again; the installed package is what makes that
# reinstall a no-op, which is also why the download is not kept in the image.
readonly wkhtmltox_sha256=ee88d74834bdec650f7432c7d3ef1c981e42ae7a762a75a01f7f5da59abc18d5

# The setup actions resolve RUNNER_TOOL_CACHE before they download anything, and
# with check-latest left at its default they take the newest version already
# present. These are the public tool distributions a hosted image ships, not a
# job's dependency cache: nothing here was produced by another job, and sealing
# does not touch /opt.
readonly tool_cache=/opt/hostedtoolcache
readonly tool_cache_arch=x64
readonly python_version=3.14.7
readonly python_archive_url=https://github.com/actions/python-versions/releases/download/3.14.7-31064857500/python-3.14.7-linux-24.04-x64.tar.gz
readonly python_archive_sha256=76d5ddab6d2dd89a39c06220f6efeda486a48ed481eae97bfc596c74ac3623db
readonly node_version=24.21.0
readonly node_archive_name=node-v${node_version}-linux-${tool_cache_arch}.tar.xz
readonly node_archive_url=https://nodejs.org/dist/v${node_version}/${node_archive_name}
readonly node_archive_sha256=fd8e59d5a511510f6a298afb548f18c7d2b1be404d8b4a27d94fbe49f56cb2d6
# The published image is truncated to 4 GiB by the finalizer, so leave the
# builder room to finish the tool cache and to clean its own scratch.
readonly published_image_bytes=4294967296
readonly image_budget_bytes=$((3584 * 1024 * 1024))

export DEBIAN_FRONTEND=noninteractive

# A private root-owned staging directory: the tool archives' own setup script and
# tar extractors must not run from a directory the unprivileged runner can write.
# Sealing empties /var/tmp, so nothing staged here can survive into the image.
staging=$(mktemp -d /var/tmp/runway-frappe.XXXXXX)
chmod 0700 "$staging"
trap 'rm -rf -- "$staging"' EXIT

# This workflow installs with `sudo apt -qq install`, which carries neither -y
# nor DEBIAN_FRONTEND, so a non-interactive runner stops at "Do you want to
# continue? [Y/n]" and aborts. Answer for every apt on this image, and keep
# existing conffiles, so the workflow itself needs no change.
cat > /etc/apt/apt.conf.d/99runway-ci <<'APT_CONF'
APT::Get::Assume-Yes "true";
Dpkg::Options { "--force-confdef"; "--force-confold"; };
APT_CONF
chown root:root /etc/apt/apt.conf.d/99runway-ci
chmod 0644 /etc/apt/apt.conf.d/99runway-ci

# gnupg and lsb-release land first because the repository setup below needs them,
# and xz-utils unpacks the Node distribution below.
apt-get update
apt-get install -y --no-install-recommends \
  build-essential pkg-config libpq-dev libcups2-dev redis-server \
  ca-certificates curl wget gnupg lsb-release xz-utils

# The vendor's own setup script is what the workflow runs. It is fetched live and
# installs MariaDB's own signing key; this recipe does not pin or verify it.
curl --fail --location --retry 3 --connect-timeout 20 --max-time 600 --silent --show-error \
  "$mariadb_repo_setup_url" | bash -s -- --mariadb-server-version="$mariadb_server_version" --skip-maxscale
apt-get update

# The workflow removes Ubuntu's MySQL metapackages and its PostgreSQL 16 client
# before installing MariaDB's client and PostgreSQL 18. An absent metapackage is
# not an error here, so only installed packages are removed.
remove_installed() {
  local package removable=()
  for package in "$@"; do
    if dpkg-query --show "$package" >/dev/null 2>&1; then
      removable+=("$package")
    fi
  done
  (( ${#removable[@]} == 0 )) || apt-get remove -y "${removable[@]}"
}
remove_installed mysql-server mysql-client postgresql-client postgresql-client-16 postgresql-client-common
apt-get install -y --no-install-recommends libmariadb-dev mariadb-client

# PGDG's key goes into a keyring named by signed-by. apt-key is deprecated and
# absent from current apt, so the workflow's apt-key call cannot be copied here.
# The repository suite comes from this guest's own codename, as lsb_release -cs
# would report, rather than from a repository that has to be installed first.
install -d -m 0755 "$(dirname "$postgres_keyring")"
curl --fail --location --retry 3 --connect-timeout 20 --max-time 600 --silent --show-error \
  "$postgres_key_url" | gpg --batch --yes --dearmor --output "$postgres_keyring"
chmod 0644 "$postgres_keyring"
printf 'deb [signed-by=%s] http://apt.postgresql.org/pub/repos/apt %s main\n' \
  "$postgres_keyring" "$postgres_suite" > /etc/apt/sources.list.d/pgdg.list
chmod 0644 /etc/apt/sources.list.d/pgdg.list
apt-get update
apt-get install -y --no-install-recommends postgresql-client-18

# Keep an image that already carries Node from being reshaped by apt's version.
# This stays the default Node outside a job; the setup action puts the tool cache
# copy ahead of it on PATH for the steps that pin a version.
if ! command -v node >/dev/null 2>&1 || ! command -v npm >/dev/null 2>&1; then
  apt-get install -y --no-install-recommends nodejs npm
fi
# Yarn's launcher resolves node from PATH when it runs, so this one copy serves
# both the distribution Node and the tool cache Node a job selects.
if [[ $(yarn --version 2>/dev/null) != 1.22.22 ]]; then
  npm install --global yarn@1.22.22
fi

# Nothing consumes this package after the install below: the workflow fetches its
# own copy, so verify the download and then delete it instead of spending part of
# the image budget on an archive no job reads.
if [[ $(dpkg-query --show --showformat='${Version}' wkhtmltox 2>/dev/null) != "$wkhtmltox_version" ]]; then
  wkhtmltox_package=$staging/wkhtmltox.deb
  curl --fail --location --retry 3 --connect-timeout 20 --max-time 600 \
    --silent --show-error --output "$wkhtmltox_package" "$wkhtmltox_url"
  printf '%s  %s\n' "$wkhtmltox_sha256" "$wkhtmltox_package" | sha256sum --check --status
  apt-get install -y "$wkhtmltox_package"
  rm -- "$wkhtmltox_package"
fi

# The tool archive's setup script deletes a version directory it finds, so this
# recipe never runs it against an entry it did not create: a complete entry is
# left alone and anything else stops the build for an operator to look at.
toolcache_usable() {
  [[ -f $1/$tool_cache_arch.complete && -x $1/$tool_cache_arch/bin/$2 ]]
}
toolcache_present() {
  [[ -e $1/$tool_cache_arch || -e $1/$tool_cache_arch.complete ]]
}

install_python_toolcache() {
  local version_path=$tool_cache/Python/$python_version
  if toolcache_present "$version_path"; then
    if toolcache_usable "$version_path" "python${python_version%.*}"; then
      printf 'Python %s is already in the tool cache; leaving it untouched.\n' "$python_version"
      return 0
    fi
    echo "Refusing to overwrite an incomplete Python $python_version tool cache entry." >&2
    exit 1
  fi
  local archive=$staging/python.tar.gz
  # An operator may have staged and verified this archive in the guest already.
  # A pre-staged copy is still checksummed here, and /var/tmp is cleared by
  # sealing, so neither path leaves the archive inside the image.
  local candidate
  for candidate in /tmp/runway-frappe/python-3.14.7.tar.gz /var/tmp/runway-frappe/python-3.14.7.tar.gz; do
    if [[ -f $candidate ]]; then
      cp -- "$candidate" "$archive"
      break
    fi
  done
  if [[ ! -f $archive ]]; then
    curl --fail --location --retry 3 --connect-timeout 20 --max-time 1800 \
      --silent --show-error --output "$archive" "$python_archive_url"
  fi
  printf '%s  %s\n' "$python_archive_sha256" "$archive" | sha256sum --check --status
  mkdir -p "$staging/python"
  tar -xzf "$archive" --no-same-owner -C "$staging/python"
  rm -- "$archive"
  install -d -m 0755 "$version_path"
  # The archive's own setup.sh installs into $RUNNER_TOOL_CACHE/Python/<version>/x64
  # and writes x64.complete. It prefers $AGENT_TOOLSDIRECTORY whenever that
  # variable is defined, even when empty, so clear it and name this cache.
  (
    cd "$staging/python"
    unset AGENT_TOOLSDIRECTORY
    RUNNER_TOOL_CACHE=$tool_cache bash ./setup.sh
  )
  if ! toolcache_usable "$version_path" "python${python_version%.*}"; then
    echo "The Python archive did not produce a complete tool cache entry." >&2
    exit 1
  fi
  "$version_path/$tool_cache_arch/bin/python${python_version%.*}" --version
}

install_node_toolcache() {
  local version_path=$tool_cache/node/$node_version
  if toolcache_present "$version_path"; then
    if toolcache_usable "$version_path" node; then
      printf 'Node %s is already in the tool cache; leaving it untouched.\n' "$node_version"
      return 0
    fi
    echo "Refusing to overwrite an incomplete Node $node_version tool cache entry." >&2
    exit 1
  fi
  local archive=$staging/node.tar.xz
  # nodejs.org publishes a digest for every release file. Require the fetched list
  # to still agree with the digest pinned above, so a republished artifact stops
  # the build instead of quietly replacing a version this image claims to ship.
  local published
  published=$(curl --fail --location --retry 3 --connect-timeout 20 --max-time 120 \
    --silent --show-error "https://nodejs.org/dist/v${node_version}/SHASUMS256.txt" |
    awk -v file="$node_archive_name" '$2 == file {print $1}')
  if [[ $published != "$node_archive_sha256" ]]; then
    echo "nodejs.org reports $published for $node_archive_name; this recipe pins $node_archive_sha256." >&2
    exit 1
  fi
  curl --fail --location --retry 3 --connect-timeout 20 --max-time 1800 \
    --silent --show-error --output "$archive" "$node_archive_url"
  printf '%s  %s\n' "$node_archive_sha256" "$archive" | sha256sum --check --status
  install -d -m 0755 "$version_path/$tool_cache_arch"
  tar -xJf "$archive" --no-same-owner --strip-components=1 -C "$version_path/$tool_cache_arch"
  rm -- "$archive"
  touch "$version_path/$tool_cache_arch.complete"
  if ! toolcache_usable "$version_path" node; then
    echo "The Node archive did not produce a complete tool cache entry." >&2
    exit 1
  fi
  "$version_path/$tool_cache_arch/bin/node" --version
}

install_python_toolcache
install_node_toolcache
# The setup actions run as the runner user and must be able to complete, extend,
# or replace an entry they manage, exactly as on a hosted image.
chown -R runner:runner "$tool_cache"

apt-get clean
rm -rf /var/lib/apt/lists/*

# Measure what the image will actually carry. The extracted tool archives and the
# downloaded packages are this script's own scratch, not image content, so the
# private staging tree goes first; the EXIT trap then has nothing left to remove.
rm -rf -- "$staging"
used_bytes=$(df -B1 --output=used / | tail -n 1 | tr -cd '0-9')
if (( used_bytes > image_budget_bytes )); then
  echo "Root filesystem holds ${used_bytes} bytes, over the ${image_budget_bytes}-byte budget for a 4 GiB image." >&2
  echo 'Give the builder a larger disk, or find which package grew the image.' >&2
  df -h /
  exit 1
fi

# Record what this image actually contains, replacing only a previous profile
# section so a re-run never duplicates lines. dpkg-query reports nothing for an
# absent package, which keeps a re-run after an optional removal honest.
profile_section=$(mktemp /var/tmp/runway-frappe-profile.XXXXXX)
{
  printf '# runway frappe profile\n'
  printf 'FRAPPE_PROFILE=1\n'
  printf 'APT_ASSUME_YES_CONFIG=/etc/apt/apt.conf.d/99runway-ci\n'
  printf 'MARIADB_SERVER_VERSION=%s\n' "$mariadb_server_version"
  printf 'MARIADB_REPO_SETUP=%s\n' "$mariadb_repo_setup_url"
  printf 'POSTGRES_CLIENT_SUITE=%s\n' "$postgres_suite"
  printf 'POSTGRES_KEYRING=%s\n' "$postgres_keyring"
  printf 'WKHTMLTOX_VERSION=%s\n' "$wkhtmltox_version"
  printf 'WKHTMLTOX_URL=%s\n' "$wkhtmltox_url"
  printf 'WKHTMLTOX_SHA256=%s\n' "$wkhtmltox_sha256"
  printf 'WKHTMLTOX_ARCHIVE_RETAINED=no (the workflow fetches its own copy)\n'
  printf 'TOOLCACHE_ROOT=%s\n' "$tool_cache"
  printf 'TOOLCACHE_ARCH=%s\n' "$tool_cache_arch"
  printf 'TOOLCACHE_PYTHON_VERSION=%s\n' "$python_version"
  printf 'TOOLCACHE_PYTHON_URL=%s\n' "$python_archive_url"
  printf 'TOOLCACHE_PYTHON_SHA256=%s\n' "$python_archive_sha256"
  printf 'TOOLCACHE_NODE_VERSION=%s\n' "$node_version"
  printf 'TOOLCACHE_NODE_URL=%s\n' "$node_archive_url"
  printf 'TOOLCACHE_NODE_SHA256=%s\n' "$node_archive_sha256"
  printf 'DISTRIBUTION_NODE_VERSION=%s\n' "$(node --version 2>/dev/null || printf absent)"
  printf 'NPM_VERSION=%s\n' "$(npm --version 2>/dev/null || printf absent)"
  printf 'YARN_VERSION=%s\n' "$(yarn --version 2>/dev/null || printf absent)"
  # Record capacity as measured on this build. A published image is truncated to
  # the size below, so the difference is what a clone starts with; it is not a
  # promise about the space a job needs.
  printf 'IMAGE_FS_BYTES=%s\n' "$published_image_bytes"
  printf 'IMAGE_BUDGET_BYTES=%s\n' "$image_budget_bytes"
  printf 'IMAGE_USED_BYTES=%s\n' "$used_bytes"
  printf 'IMAGE_FREE_BYTES_AT_PUBLISH=%s\n' "$((published_image_bytes - used_bytes))"
  dpkg-query --show --showformat='${binary:Package}=${Version}\n' \
    build-essential pkg-config libmariadb-dev libpq-dev libcups2-dev redis-server \
    mariadb-client postgresql-client-18 curl wget gnupg lsb-release nodejs npm \
    wkhtmltox xz-utils 2>/dev/null || true
} > "$profile_section"

# The guest installer owns the first lines of this file; keep them and swap only
# the profile section underneath them.
rebuilt=$(mktemp /var/tmp/runway-runner-image.XXXXXX)
awk '/^# runway frappe profile$/{exit} {print}' "$manifest" > "$rebuilt"
cat "$profile_section" >> "$rebuilt"
rm -f -- "$profile_section"
install -m 0644 -o root -g root "$rebuilt" "$manifest"
rm -f -- "$rebuilt"

# These prove the guest is usable as a job host, including the tool cache entries
# a setup action would otherwise download. None of it creates a GitHub resource,
# a runner registration, or a database.
python_major_minor=${python_version%.*}
"$tool_cache/Python/$python_version/$tool_cache_arch/bin/python$python_major_minor" --version
"$tool_cache/node/$node_version/$tool_cache_arch/bin/node" --version
# The workflow's yarn lives outside the tool cache, so resolve node the way a job
# does and prove the two together before the image is sealed.
PATH=$tool_cache/node/$node_version/$tool_cache_arch/bin:$PATH yarn --version
wkhtmltopdf --version
psql --version
mariadb --version
df -h /
echo 'Frappe dependency profile prepared. Seal the image next; it still has no credentials.'
