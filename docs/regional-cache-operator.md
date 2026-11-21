# Operate the regional Runway cache

Create one cache VM per region. Build the operator-only `runway-cache`
one-click template with a pinned HTTPS URL and SHA-256 for the tarball from
`scripts/build-cache-service-release.sh`. The template requires at least 8 GiB of disk.
Use 20 GiB or more in production for cache-data state and spools. Each Team's objects live in its Cargo
bucket.

## Reconcile on the controller host

1. Migrate the Runway site and create a **Runway Region** record matching each
   configured region. Give a dedicated local API user only the **Runway Cache
   Provisioner** role. Save its Frappe `api_key:api_secret` in a root-owned
   mode-0600 file. Save the regional Atlas operator bearer token in a separate
   root-owned mode-0600 file on the controller host. The service rereads these
   files on each request.
2. Install [the example config](../deploy/regional-cache/cache-reconcile.example.json)
   as `/etc/runway/cache-reconcile.json`, owned by root with mode 0600. Set the
   Runway and Atlas HTTPS origins, image ID and VM shape for each region. Keep
   the mesh-only firewall in the example. For private TLS trust, set
   `runway_site_ca_base64` or `runway_storage_ca_base64` to the base64-encoded
   public PEM bundle. The former also configures the reconciler's trust in
   the Runway site.
3. Install [runway-cache-reconcile.service](../deploy/regional-cache/systemd/runway-cache-reconcile.service)
   on the controller host and enable it with
   `systemctl enable --now runway-cache-reconcile.service`. Install the service
   on one controller host and preserve its state directory across restarts.
   For a custom `state_dir`, create the directory with root ownership and mode 0700.
4. Confirm that Runway Region lists the VM's mesh endpoint and public CA and
   that the token is consumed. Enable regional cache on the pool. See
   [monitoring](monitoring.md) for the reconcile metrics.

The service tags its VMs with `role=runway-cache`, `region=<region>` and a
unique attempt ID. It waits two minutes for an unconfirmed create to become
visible. If enrolment takes longer than ten minutes or the VM goes down or
drifts from its configured shape, it revokes the cache credential, terminates
the journalled VM, and waits for Atlas to confirm absence before creating
again. During replacement jobs can fall back to GitHub's cache.

If duplicate tagged VMs appear, the service reports `duplicate_vms` and
pauses writes for the region. Check the journal for the tracked VM and resolve
the duplicate in Atlas. An existing manually managed VM or enrolment requires
operator migration. Revoke and terminate the manual VM, confirm absence, then
start the service. Stop the reconciler before manually changing the region's
enrolment or switching its Runway or Atlas origin. Atlas and Metal receive the
one-use token in cloud-init user data. Treat access to that user data as
sensitive until the token is consumed or expires.

## Manual fallback

Stop the reconciler for the region before using the manual procedure.

### Create and enrol

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

### Rotate, revoke or replace

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
