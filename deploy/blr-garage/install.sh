#!/bin/bash
# Install or upgrade the persistent regional-cache services on blr-1.
#   install.sh /path/to/runway   (a linux/amd64 build of this repository)
# Idempotent. Keeps the previous binaries as *.previous for rollback. Never
# touches metal.service, metald state or its ZFS pool.
set -euo pipefail
umask 077
here=$(cd "$(dirname "$0")" && pwd)
runway=${1:?usage: install.sh /path/to/runway-binary}
[ "$(id -u)" = 0 ] || { echo "run as root" >&2; exit 1; }
# shellcheck source=garage.pin
. "$here/garage.pin"

id runway-garage >/dev/null 2>&1 || useradd --system --no-create-home --home-dir /var/lib/runway-garage --shell /usr/sbin/nologin runway-garage
install -d -m 0755 /opt/runway-garage /opt/runway-garage/bin /opt/runway-cache /opt/runway-cache/bin /opt/runway-cache/guest
install -d -m 0700 /etc/runway-cache /etc/runway-cache/tls /var/lib/runway-cache /var/lib/runway-cache/spools
install -d -m 0700 -o runway-garage -g runway-garage /etc/runway-garage /var/lib/runway-garage /var/lib/runway-garage/meta /var/lib/runway-garage/data

# replace SOURCE TARGET MODE: install atomically, keeping the old file as TARGET.previous.
replace() {
	if [ -e "$2" ] && ! cmp -s "$1" "$2"; then cp -p "$2" "$2.previous"; fi
	install -m "$3" "$1" "$2.new"
	mv -f "$2.new" "$2"
}

# Garage: download only when the pinned binary is not already installed.
if [ "$(sha256sum /opt/runway-garage/bin/garage 2>/dev/null | cut -d' ' -f1)" != "$GARAGE_SHA256" ]; then
	download=$(mktemp -d)
	trap 'rm -rf "$download"' EXIT
	curl -fsSL --proto '=https' --max-time 300 -o "$download/garage" "$GARAGE_URL"
	echo "$GARAGE_SHA256  $download/garage" | sha256sum -c --quiet -
	replace "$download/garage" /opt/runway-garage/bin/garage 0755
fi
# No pipe into grep -q: under pipefail its early exit fails the check.
version=$(/opt/runway-garage/bin/garage --version)
case $version in "garage $GARAGE_VERSION"|"garage $GARAGE_VERSION ["*) ;; *) echo "unexpected Garage version: $version" >&2; exit 1 ;; esac
# The new runway must run here and know the cache actions before it replaces the old one.
check=$("$runway" --action cache-data 2>&1 || true)
case $check in *"cache-data requires --config"*) ;; *) echo "runway binary refused: $check" >&2; exit 1 ;; esac

replace "$runway" /opt/runway-cache/bin/runway 0755
replace "$here/health.sh" /opt/runway-cache/bin/runway-cache-health 0755
replace "$here/../standalone-guest/runway-standalone-guest" /opt/runway-cache/bin/runway-standalone-guest 0755
replace "$here/../standalone-guest/setup-probe.py" /opt/runway-cache/bin/runway-setup-probe 0755
# This branch's guest cache helpers; the launcher copies them into each guest.
for helper in cache-relay cache-setup; do
	replace "$here/../../integrations/actions-runner/$helper" "/opt/runway-cache/guest/$helper" 0755
done
replace "$here/../standalone-guest/runway-cache-relay.service" /opt/runway-cache/guest/runway-cache-relay.service 0644
install -m 0600 -o runway-garage -g runway-garage "$here/garage.toml" /etc/runway-garage/garage.toml
for unit in "$here"/systemd/*; do install -m 0644 "$unit" /etc/systemd/system/; done
install -m 0644 "$here/tmpfiles.conf" /etc/tmpfiles.d/runway-cache.conf
systemd-tmpfiles --create /etc/tmpfiles.d/runway-cache.conf

# Generated secrets stay on the host in Garage-only files; they are never printed.
for name in rpc-secret admin-token metrics-token; do
	if [ ! -s "/etc/runway-garage/$name" ]; then
		if [ "$name" = rpc-secret ]; then openssl rand -hex 32; else openssl rand -base64 32; fi > "/etc/runway-garage/$name.tmp"
		mv "/etc/runway-garage/$name.tmp" "/etc/runway-garage/$name"
	fi
done
chown -R runway-garage:runway-garage /etc/runway-garage /var/lib/runway-garage
chmod 0600 /etc/runway-garage/*

# Migrate only the retired standalone keys; preserve storage, keys and journal.
python3 -I - <<'PYMIGRATE'
import json, os, shutil
for name in ('cache-data.json', 'host.json'):
    path = '/etc/runway-cache/' + name
    if not os.path.exists(path):
        continue
    with open(path) as source:
        data = json.load(source)
    before = json.dumps(data, sort_keys=True)
    if name == 'cache-data.json':
        if 'garage_fixture' in data:
            if 'garage' in data:
                raise ValueError('both Garage configuration keys present')
            data['garage'] = data.pop('garage_fixture')
        data.pop('clock_source', None)
        data.pop('lifetime_seconds', None)
        for field in ('archive_bytes', 'storage_bytes'):
            data.get('disk', {}).pop(field, None)
    else:
        data.pop('team_id', None)
    if json.dumps(data, sort_keys=True) == before:
        continue
    shutil.copy2(path, path + '.previous')
    with open(path + '.new', 'w', opener=lambda p, flags: os.open(p, flags, 0o600)) as target:
        json.dump(data, target)
        target.flush()
        os.fsync(target.fileno())
    os.replace(path + '.new', path)
PYMIGRATE

systemctl daemon-reload
systemctl enable runway-garage.service
systemctl restart runway-garage.service
# Restart every consumer after replacing the binary.
for unit in runway-cache-data.service runway-cache-assignments.service; do
	if systemctl is-enabled --quiet "$unit" 2>/dev/null; then systemctl restart "$unit"; fi
done
while read -r unit _; do
    [ -n "$unit" ] && systemctl restart "$unit"
done < <(systemctl list-units --no-legend --state=active 'runway-cache-host@*')
systemctl enable --now runway-cache-health.timer
echo "installed: garage $version runway $(sha256sum /opt/runway-cache/bin/runway | cut -c1-16)…"
