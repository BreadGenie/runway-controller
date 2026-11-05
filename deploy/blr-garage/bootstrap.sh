#!/bin/bash
# One-time (idempotent) bootstrap after install.sh: Garage layout, bucket and
# bucket-scoped key; cache-data TLS; grant signing key; service configuration.
# Secrets are written only to root-only files and never printed.
# Requires the host GitHub credential at /etc/runway-cache/github-token (0600).
set -euo pipefail
umask 077
[ "$(id -u)" = 0 ] || { echo "run as root" >&2; exit 1; }
G=(/opt/runway-garage/bin/garage -c /etc/runway-garage/garage.toml)
bucket=runway-cache
key_name=runway-cache-data
capacity=${RUNWAY_GARAGE_CAPACITY_BYTES:-42949672960} # 40 GiB of the root filesystem
region=${RUNWAY_CACHE_REGION:-blr-1}
# The runner scale set whose JIT runners the launcher registers (runs-on label).
repository_url=${RUNWAY_REPOSITORY_URL:-https://github.com/BreadGenie/frappe}
scale_set_id=${RUNWAY_SCALE_SET_ID:-}
etc=/etc/runway-cache

for _ in $(seq 30); do "${G[@]}" json-api GetClusterStatus >/dev/null 2>&1 && break; sleep 1; done
json() { python3 -I -c "import json,sys; d=json.load(sys.stdin); print($1)"; }

# Layout: one node, one zone, replication factor 1.
if [ "$("${G[@]}" json-api GetClusterLayout | json 'len(d["roles"])')" = 0 ]; then
	node=$("${G[@]}" json-api GetClusterStatus | json 'd["nodes"][0]["id"]')
	printf '{"roles":[{"id":"%s","zone":"blr1","capacity":%s,"tags":["runway-cache"]}]}' "$node" "$capacity" |
		"${G[@]}" json-api UpdateClusterLayout - >/dev/null
	"${G[@]}" json-api ApplyClusterLayout '{"version":1}' >/dev/null
fi
for _ in $(seq 30); do curl -fsS -o /dev/null http://127.0.0.1:3903/health && break; sleep 1; done

# Bucket and a key allowed only on that bucket.
if ! "${G[@]}" json-api GetBucketInfo "{\"globalAlias\":\"$bucket\"}" >/dev/null 2>&1; then
	"${G[@]}" json-api CreateBucket "{\"globalAlias\":\"$bucket\"}" >/dev/null
fi
bucket_id=$("${G[@]}" json-api GetBucketInfo "{\"globalAlias\":\"$bucket\"}" | json 'd["id"]')
if [ ! -s "$etc/garage-key.json" ]; then
	"${G[@]}" json-api CreateKey "{\"name\":\"$key_name\"}" |
		python3 -I -c 'import json,sys; d=json.load(sys.stdin); json.dump({"access_key":d["accessKeyId"],"secret_key":d["secretAccessKey"]},sys.stdout)' > "$etc/garage-key.json.tmp"
	mv "$etc/garage-key.json.tmp" "$etc/garage-key.json"
fi
access_key=$(json 'd["access_key"]' < "$etc/garage-key.json")
printf '{"bucketId":"%s","accessKeyId":"%s","permissions":{"read":true,"write":true,"owner":false}}' "$bucket_id" "$access_key" |
	"${G[@]}" json-api AllowBucketKey - >/dev/null

# cache-data TLS: a private CA whose key is discarded after signing one leaf.
if [ ! -s "$etc/tls/cache-data.pem" ]; then
	work=$(mktemp -d); trap 'rm -rf "$work"' EXIT
	openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 1825 -subj "/CN=runway-cache-data CA $region" \
		-keyout "$work/ca.key" -out "$work/ca.pem" -addext basicConstraints=critical,CA:TRUE -addext keyUsage=critical,keyCertSign 2>/dev/null
	openssl req -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -subj "/CN=127.0.0.1" -keyout "$work/leaf.key" -out "$work/leaf.csr" 2>/dev/null
	printf 'subjectAltName=IP:127.0.0.1\nbasicConstraints=critical,CA:FALSE\nkeyUsage=critical,digitalSignature\nextendedKeyUsage=serverAuth\n' > "$work/leaf.ext"
	openssl x509 -req -in "$work/leaf.csr" -CA "$work/ca.pem" -CAkey "$work/ca.key" -CAcreateserial -days 1825 -extfile "$work/leaf.ext" -out "$work/leaf.pem" 2>/dev/null
	install -m 0644 "$work/ca.pem" "$etc/tls/ca.pem"
	install -m 0600 "$work/leaf.key" "$etc/tls/cache-data.key"
	install -m 0644 "$work/leaf.pem" "$etc/tls/cache-data.pem"
fi
chmod 0700 "$etc/tls"

# Grant signing key (Ed25519, PKCS#8). cache-data receives only its public key.
if [ ! -s "$etc/signing-key.pem" ]; then
	openssl genpkey -algorithm ed25519 -out "$etc/signing-key.pem.tmp"
	mv "$etc/signing-key.pem.tmp" "$etc/signing-key.pem"
fi
public=$(openssl pkey -in "$etc/signing-key.pem" -pubout -outform DER | tail -c 32 | od -An -tx1 | tr -d ' \n')
key_id=$(openssl pkey -in "$etc/signing-key.pem" -pubout -outform DER | tail -c 32 | sha256sum | cut -d' ' -f1)

python3 -I - "$etc" "$key_id" "$public" "$region" "$bucket" "$repository_url" "$scale_set_id" <<'PY'
import json, os, sys
etc, key_id, public, region, bucket, repository_url, scale_set_id = sys.argv[1:]
key = json.load(open(os.path.join(etc, "garage-key.json")))
data = {
    "version": 1, "listen": "127.0.0.1:18445",
    "public_origin": "https://127.0.0.1:18445",
    "certificate_file": etc + "/tls/cache-data.pem", "private_key_file": etc + "/tls/cache-data.key",
    "public_keys": {key_id: public}, "region_id": region,
    "state_dir": "/var/lib/runway-cache/state",
    "quota_bytes": 4294967296, "total_bytes": 4294967296, "max_archive_bytes": 1073741824,
    "disk": {"directory": "/var/lib/runway-cache/spools", "physical_bytes": 4294967296},
    "storage_budget": {"base_seconds": 12, "min_bytes_per_second": 16777216, "max_seconds": 300, "recovery_max_seconds": 900},
    "garage": {"endpoint": "http://127.0.0.1:3900", "region": "garage", "bucket": bucket, "access_key": key["access_key"], "secret_key": key["secret_key"]},
}
host = {
    "version": 1, "region_id": region, "audience": "https://127.0.0.1:18445",
    "cache_data_ca_file": etc + "/tls/ca.pem", "signing_key_file": etc + "/signing-key.pem",
    "github_token_file": etc + "/github-token", "binding_dir": "/run/runway-cache/bindings",
    "vm_dir": "/run/runway-cache/vms", "assignment_dir": "/run/runway-cache/assignments",
    "archive_bytes": 1073741824,
}
files = [("cache-data.json", data), ("host.json", host)]
if scale_set_id:
    files.append(("assignments.json", {"version": 1, "repository_url": repository_url, "scale_set_id": int(scale_set_id),
                                       "github_token_file": etc + "/github-token", "assignment_dir": "/run/runway-cache/assignments", "max_runners": 4}))
for name, value in files:
    path = os.path.join(etc, name)
    with open(path + ".tmp", "w", opener=lambda p, f: os.open(p, f, 0o600)) as out:
        json.dump(value, out, separators=(",", ":"))
    os.replace(path + ".tmp", path)
PY

[ -s "$etc/github-token" ] || echo "warning: install the host GitHub credential at $etc/github-token (0600) before launching guests" >&2
systemctl enable --now runway-cache-data.service runway-cache-health.timer
[ -s "$etc/assignments.json" ] && systemctl enable --now runway-cache-assignments.service
sleep 2
/opt/runway-cache/bin/runway-cache-health
