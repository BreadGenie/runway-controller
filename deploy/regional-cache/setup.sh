#!/bin/bash
# Atlas invokes without arguments. No downloads or package-manager operations.
set -euo pipefail
umask 077
[[ $# == 0 ]] || { echo 'setup takes no arguments' >&2; exit 2; }
root=${RUNWAY_ROOT:-}
config=${RUNWAY_CONFIG:-/etc/runway/cache-service.json}
storage=${RUNWAY_STORAGE_CA_FILE:-/etc/runway/storage-ca.pem}
: "${RUNWAY_DOMAIN:?RUNWAY_DOMAIN required}"
[[ -n $root || $(id -u) == 0 ]] || { echo 'run as root' >&2; exit 1; }
here=$(cd "$(dirname "$0")" && pwd)
# Use the released decoder, including strict field, mesh and listener checks.
"$here/bin/runway" --action cache-service-check --config "$root$config"
[[ $RUNWAY_DOMAIN =~ ^[a-zA-Z0-9][a-zA-Z0-9.-]*$ && $storage =~ ^/[a-zA-Z0-9/._-]+$ ]] || exit 2
changed=0
replace() {
 if [[ -f $2 ]] && cmp -s "$1" "$2"; then return; fi
 if [[ -e $2 ]]; then cp -p "$2" "$2.previous"; fi
 install -m "$3" "$1" "$2.new"
 mv -f "$2.new" "$2"
 changed=1
}
install -d -m 0755 "$root/usr/local/bin" "$root/etc/systemd/system"
install -d -m 0700 "$root/etc/runway" "$root/etc/runway/tls" "$root/var/lib/runway-cache" "$root/var/lib/runway-cache/spools"
chmod 0600 "$root$config"
if [[ $config != /etc/runway/cache-service.json ]]; then
 replace "$root$config" "$root/etc/runway/cache-service.json" 0600
fi
replace "$here/bin/runway" "$root/usr/local/bin/runway" 0755
for unit in "$here"/systemd/*; do replace "$unit" "$root/etc/systemd/system/${unit##*/}" 0644; done
tmp=$(mktemp)
trap 'rm -f "$tmp"' EXIT
printf 'RUNWAY_DOMAIN=%s\nRUNWAY_STORAGE_CA_FILE=%s\n' "$RUNWAY_DOMAIN" "$storage" > "$tmp"
replace "$tmp" "$root/etc/runway/release.env" 0600
# Track config contents too: Atlas rewrites them independently of the release.
sha256sum "$root$config" "$here/bin/runway" "$here"/systemd/* "$root/etc/runway/release.env" | sha256sum | cut -d' ' -f1 > "$tmp"
if ! cmp -s "$tmp" "$root/etc/runway/release-config.sha256"; then changed=1; fi
if [[ $changed == 1 ]]; then
 systemctl daemon-reload
 systemctl enable runway-cache-data.service runway-cache-service.service
 systemctl restart runway-cache-data.service runway-cache-service.service
fi

replace "$tmp" "$root/etc/runway/release-config.sha256" 0600
