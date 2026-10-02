# Build a reusable runner image

Bake the official runner and Docker into a fresh Ubuntu guest before registration. Each job then boots a fresh clone and receives its own JIT configuration. The image contains no GitHub App key, runner registration, job workspace, or control-plane credential.

## Inputs and capacity

Use an Available Atlas Ubuntu 24.04 x86_64 System image with the Docker-capable kernel from [the kernel recipe](../scripts/build-ci-kernel.sh). The current installer pins the runner archive and checksum, but Ubuntu package versions depend on the package repositories at build time.

The procedure needs an operator-authorized Central/Atlas environment, administrative SSH access to the Metal host, and a local Linux machine with `e2fsprogs`, `zerofree`, and `zstd`. Keep exported images outside this repository. Allow at least 10 GiB of local space for the raw filesystem, compressed artifact, and working copies.

The tested builder uses one vCPU, 2 GiB RAM, and a 4 GiB disk. On a small Metal pool, a cached base image reserves more than 4 GiB. Do not keep both old and new image caches when that exceeds available capacity. Finish and delete the builder and its staging before booting a verification guest. Use Metal's guarded unused-image pruning while no VM uses the old cache; never destroy a live image dataset.

## Install and seal a builder

Create a new VM through `scripts/atlas-vm.py`, using an explicit source image and a new creation journal. The builder must never receive JIT material or run a GitHub job. A used runner cannot become a shared image, even if its registration files are later removed.

Copy the guest installer and sealing script into the builder. Run these commands as root **inside that guest**:

```sh
bash install-guest-runner.sh --inside-disposable-guest
bash seal-runner-image.sh --inside-unregistered-image-builder
```

The installer checks runner execution and Docker access. The sealing script refuses a registered runner, stops guest services, clears Docker state, SSH identities, cloud-init instance data, machine identity, logs, package caches, and user history. It leaves Docker enabled for a new clone's first boot.

Stop the builder through Central's `AtlasClient.vm_action(vm_id, "stop")`. Poll until its observed state is stopped. Do not restart or register the sealed builder.

## Capture and export

Atlas's normal Machine image transfer uses object storage. For a bootstrap region without object storage, capture a stopped disk through Metal's snapshot API and publish the resulting sanitized filesystem as a System Site File image. Site File images are public artifacts, so this path is restricted to credential-free System images.

From an initialized Atlas administrator context, resolve the VM's Metal Server and call:

```python
from atlas.vm.core.metal_client import MetalClient

client = MetalClient(server)
snapshot = client.create_snapshot(vm_id)
```

Persist the returned snapshot ID before continuing. Metal stages a read-only ZFS clone and the kernel. On the Metal host, stream `/dev/zvol/metal/staging/<snapshot-id>` through `dd` and `zstd` over the administrative SSH connection to local storage. Do not write another full raw image onto a small host root disk. Read the block device through `dd`; passing a device symlink directly to `zstd` can produce no output.

Verify the compressed stream, then decompress it to an ordinary local file. Finalize that unmounted export:

```sh
zstd --test /private/runner.ext4.zst
zstd --decompress /private/runner.ext4.zst --output /private/runner.ext4
bash scripts/finalize-runner-image.sh /private/runner.ext4
```

The finalizer checks sealing preconditions, repairs the filesystem, resizes it to 4 GiB before truncation, replaces the ext4 journal, and zeroes unused blocks. The last two steps remove residual deleted identity material from free blocks and the old journal. It then writes a fresh compressed artifact and prints both hashes. Never run it against a mounted filesystem or a live VM disk.

After a successful export, delete staging through `client.delete_snapshot(snapshot_id)`. Terminate the builder through the Central helper and wait for tenant-scoped `404`.

## Publish and verify

Use Atlas's existing System image publisher in an initialized administrator context:

```python
from pathlib import Path
from atlas.vm.core.image_builder import publish_ubuntu_image

publish_ubuntu_image(
    "Runway Ubuntu 24.04 runner 2.337.0",
    "24.04",
    "amd64",
    Path("/private/runner.ext4"),
    Path("/private/vmlinux-5.10.223-fc-ci"),
    storage="Site File",
)
```

The publisher expects `runner.ext4.zst` beside the raw filesystem. Commit the transaction, record the new image ID and digests, and confirm it is Available and enabled. Use a distinct title when preserving an earlier image for rollback; publishing a changed image under the same title retires its predecessor.

Boot a second fresh VM from the published image. Without installing packages, check that the runner prints its expected version, Docker responds as the runner user, a published Docker Redis port returns `PONG`, and cloud-init generated fresh SSH host keys and machine identity. Confirm that `.runner` and `.credentials` are absent before any JIT handoff. Delete this verification VM after validation. Retain its test evidence and the image's package manifest at `/etc/runway-runner-image.txt`.

The resulting image accelerates bootstrap. It does not implement GitHub-hosted image parity, automatic package updates, image signing, or an image promotion policy.
