#!/bin/bash
# Health check for the persistent regional-cache services. Prints one JSON
# line and exits non-zero when any check fails. Run by runway-cache-health.timer
# or by hand: /opt/runway-cache/bin/runway-cache-health
set -uo pipefail
failures=()
check() { local name=$1; shift; "$@" >/dev/null 2>&1 || failures+=("$name"); }
loopback_only() {
	# Every listener on the port must be bound to 127.0.0.1.
	local lines
	lines=$(ss -Hltn "sport = :$1") || return 1
	[ -n "$lines" ] && ! awk '{print $4}' <<<"$lines" | grep -vq '^127\.0\.0\.1:'
}
check garage_active systemctl is-active --quiet runway-garage.service
check cache_data_active systemctl is-active --quiet runway-cache-data.service
check garage_health curl -fsS --max-time 5 http://127.0.0.1:3903/health
check garage_status /opt/runway-garage/bin/garage -c /etc/runway-garage/garage.toml status
check cache_data_tls curl -sS --max-time 5 -o /dev/null --cacert /etc/runway-cache/tls/ca.pem https://127.0.0.1:18445/
for port in 3900 3901 3903 18445; do check "loopback_$port" loopback_only "$port"; done
check ntp_synchronized test "$(timedatectl show -p NTPSynchronized --value)" = yes
usage=$(df --output=pcent /var/lib/runway-garage | tail -1 | tr -dc 0-9)
[ "${usage:-100}" -lt 90 ] || failures+=("disk_usage")
data_bytes=$(du -sb /var/lib/runway-garage/data 2>/dev/null | cut -f1)
status=ok; [ ${#failures[@]} -eq 0 ] || status=failed
printf '{"status":"%s","failures":"%s","garage_data_bytes":%s,"filesystem_used_percent":%s}\n' "$status" "${failures[*]}" "${data_bytes:-0}" "${usage:-0}"
[ "$status" = ok ]
