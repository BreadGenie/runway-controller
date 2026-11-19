# Operate the regional Runway cache

Create one cache VM per region. Build the operator-only `runway-cache`
one-click template with a pinned HTTPS URL and SHA-256 for the tarball from
`scripts/build-cache-service-release.sh`. Give the VM at least 20 GiB of
disk for cache-data state and spools. Each Team's objects live in its Cargo
bucket.

## Create and enrol

1. On the Runway site, create the **Runway Region** record matching the pool's
   region name. As System Manager, select **Issue Cache Enrolment Token**. The
   token expires in 15 minutes and is consumed by a successful enrolment.
2. Use the regional Atlas operator API in tenant 0 to create a VM from the
   built image with `is_privileged: true`. Leave IPv4 internet access at its
   default `true`. Omit public IP allocations. Set the firewall on create:

   ```json
   {
     "enabled": true,
     "inbound": [
       {"protocol": "tcp", "ports": "8443", "cidrs": ["fdaa::/16"]},
       {"protocol": "icmp", "cidrs": ["fdaa::/16"]}
     ],
     "outbound": [
       {"protocol": "any", "cidrs": ["0.0.0.0/0", "::/0"]}
     ]
   }
   ```

3. Supply cloud-config user data with a root-owned mode-0600
   `/etc/one-click/inputs.env`. Set `RUNWAY_URL` to the Runway site's HTTPS
   origin, `RUNWAY_REGION` to the Runway Region name and
   `RUNWAY_ENROLMENT_TOKEN` to the token. For private TLS trust, add
   `RUNWAY_SITE_CA_BASE64` or `RUNWAY_STORAGE_CA_BASE64` with the relevant
   public PEM bundle encoded as base64. Keep the user data out of shell history
   and logs. The VM needs outbound access to the Runway site and Cargo endpoint.
4. Check that Runway Region lists the VM's `[mesh-address]:8443` and public
   CA, the two Runway cache units are active, and `/var/lib/one-click/ready`
   exists. Enable regional cache on the pool. Use a runner image with
   mesh-address support in its guest agent, cache node and relay.
   For development qualification with an older baked image, the Metal guest
   transport accepts `--regional-cache-node` and `--regional-cache-relay`
   pointing to compatible helpers on the controller. Add them to the controller
   configuration's `transport_command`. They apply during cache-enabled preparation.

## Rotate, revoke or replace

Run `sudo runway-controller --action cache-service-rotate --config /etc/runway/cache-service.json`
on the cache VM. If the reply is interrupted, repeat the same command. Both
services read the credential from disk. Runway rejects the old credential as
soon as rotation succeeds.

Select **Revoke Cache Credential** on the Runway Region to clear its address,
CA, credential and pending token. New cache sessions fall back to GitHub.
Existing grants and download tickets retain their normal expiry.

For replacement, revoke, terminate the old VM through Atlas and create a new
VM with a fresh token. For re-enrolment on the same VM, revoke first, remove
`/etc/runway/credential` and `/etc/runway/credential.pending`, then write a
fresh token to `/etc/runway/enrolment-token` as a root-owned mode-0600 file.
Run `cache-service-enrol` using the same binary and config, remove the one-use
token file after success, and verify the Runway Region record again. The
Team buckets stay in Cargo.
