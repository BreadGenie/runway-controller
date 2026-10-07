# Build a reusable runner image

Bake the official runner and Docker into a fresh Ubuntu guest before registration. Each job then boots a fresh clone and receives its own JIT configuration. The image contains no GitHub App key, runner registration, job workspace, or control-plane credential.

## Inputs and capacity

Use an Available Atlas Ubuntu 24.04 x86_64 System image with the Docker-capable kernel from [the kernel recipe](../scripts/build-ci-kernel.sh). The current installer pins the runner archive and checksum, but Ubuntu package versions depend on the package repositories at build time.

The procedure needs an operator-authorized Central/Atlas environment, administrative SSH access to the Metal host, and a local Linux machine with `e2fsprogs`, `zerofree`, and `zstd`. Keep exported images outside this repository. Allow at least 10 GiB of local space for the raw filesystem, compressed artifact, and working copies.

The tested builder uses one vCPU, 2 GiB RAM, and a 4 GiB disk. On a small Metal pool, a cached base image reserves more than 4 GiB. Do not keep both old and new image caches when that exceeds available capacity. Finish and delete the builder and its staging before booting a verification guest. Use Metal's guarded unused-image pruning while no VM uses the old cache; never destroy a live image dataset.

## Install and seal a builder

Create a new VM through `scripts/atlas-vm.py`, using an explicit source image and a new creation journal. The builder must never receive JIT material or run a GitHub job. A used runner cannot become a shared image, even if its registration files are later removed.

Copy the guest installer, the sealing script, and any profile script into the builder. Run these commands as root **inside that guest**:

```sh
bash install-guest-runner.sh --inside-disposable-guest
bash seal-runner-image.sh --inside-unregistered-image-builder
```

The installer checks runner execution and Docker access. The sealing script refuses a registered runner, stops guest services, clears Docker state, SSH identities, cloud-init instance data, machine identity, logs, package caches, and user history. It leaves Docker enabled for a new clone's first boot.

To ship an image that a Frappe workflow can use directly, add the dependency profile between the installer and the sealing script. See [Add the Frappe dependency profile](#add-the-frappe-dependency-profile). The same builder can also bake the workflow's service container images; see [Add the service container images](#add-the-service-container-images).

Stop the builder through Central's `AtlasClient.vm_action(vm_id, "stop")`. Poll until its observed state is stopped. Do not restart or register the sealed builder.

## Add the Frappe dependency profile

A Frappe job installs its own system dependencies on every run through `.github/actions/setup`, and on a fresh clone those installs dominate the run. [`guest/prepare-frappe-image.sh`](../guest/prepare-frappe-image.sh) bakes the same dependency set into a builder, so a job starts with them already present. It adds to the base image rather than replacing it: a Docker-only workload still boots an image that carries the profile.

Run it as root inside the same builder, after the guest installer and before the sealing script:

```sh
bash prepare-frappe-image.sh --inside-unregistered-image-builder
```

The script refuses any guest whose runner is registered, whose runner directory holds registration state, or that carries a JIT bootstrap marker, and it refuses to run outside a root shell on Ubuntu 24.04 x86_64. It is safe to repeat on one builder, because every stage either skips what is present or replaces only the section it owns. A background `apt` or yarn install already running in that builder still holds the dpkg lock; let it finish before starting this.

The package set mirrors the workflow's own list: build-essential, pkg-config, libmariadb-dev, libpq-dev, libcups2-dev, redis-server, mariadb-client, postgresql-client-18, curl, wget, gnupg, lsb-release, Ubuntu's nodejs and npm, and yarn 1.22.22. MariaDB's repository is set to 11.8.5 by the vendor's own setup script, invoked exactly as the workflow invokes it, and Ubuntu's MySQL metapackages and PostgreSQL 16 client are removed first so the image carries one client per database. PGDG's key is dearmored into a keyring that `signed-by` names, because `apt-key` is deprecated and absent from current apt and the workflow's `apt-key add` cannot be copied. The file `/etc/apt/apt.conf.d/99runway-ci` answers the workflow's own `sudo apt -qq install`, which carries neither `-y` nor `DEBIAN_FRONTEND` and would otherwise stop at its confirmation prompt, and it keeps existing conffiles.

The workflow also resolves its toolchain through `RUNNER_TOOL_CACHE` before downloading anything, and with `check-latest` left at its default it takes the newest version already present. The script therefore installs Python 3.14.7 from the official `actions/python-versions` archive and Node 24.21.0 from nodejs.org under `/opt/hostedtoolcache`, each in the `x64` layout with its `x64.complete` marker, owned by the runner user so a setup action can still extend or replace an entry it manages. The archive's own installer deletes a version directory it finds, so the script never runs it against an entry it did not create: a complete entry is left alone and anything else stops the build. Both archives are checked against digests pinned in the script, and the Node digest is additionally cross-checked against the digest nodejs.org currently publishes, so a republished artifact stops the build instead of quietly changing what the image claims to ship. An operator who already staged and verified the Python archive in the guest may leave it in `/tmp/runway-frappe/`, and that copy is used and checksummed again. These are the public tool distributions a hosted image ships, not a job's dependency cache: nothing in them was produced by another job, and sealing leaves `/opt` alone, so both entries reach every clone. The distribution nodejs stays the default Node outside a job, the setup action puts the tool cache copy ahead of it on PATH for the steps that pin a version, and the single yarn installed from npm serves both because its launcher resolves `node` from PATH when it runs.

Only the runner archive and those three tool archives are pinned. The MariaDB setup script, the PGDG key, and every apt package are fetched live at build time, are not checksum-verified by this recipe, and depend on the repositories as they are that day. Rebuilding is how an image picks up repository changes, so read `/etc/runway-runner-image.txt` from the published image to learn what a given build shipped. The `wkhtmltox` package is the third pinned archive: the script downloads it from the same official release, checks it against its digest, installs it, and deletes the download, because the workflow fetches its own copy at `/tmp/wkhtmltox.deb` and no job reads an archive from inside the image. Sealing empties `/tmp`, so a clone's first job takes the cache-miss branch and downloads it again; the installed package is what makes that reinstall a no-op.

The builder needs room for the finished image plus this recipe's scratch, because the tool archives are downloaded and extracted before anything is measured: the build that produced the published profile image used a 14 GiB guest. The 4 GiB builder described above is enough for the runner-only image and not for this profile, because the tool cache is written and expanded inside the guest before the finalizer truncates anything. The script measures the root filesystem after cleaning the apt cache and after deleting its own private staging tree, so the measurement is the image's own content rather than this recipe's scratch, and it fails when that exceeds the budget, currently 8 GiB against a 10 GiB published size. That gate is only about what the finalizer can truncate; it is not a claim about the space a job needs, and it does not tell you how large the builder disk must be.

Size the pool separately from the artifact. The published size is a truncation target, not a capacity plan: a pool serving Frappe needs 14336 MiB per guest, and the guest transport grows the sealed filesystem onto that larger disk at boot. The manifest records the published filesystem size, the budget, the bytes this build used, and the free space that leaves, so the capacity of a given image is a recorded measurement rather than a promise, and an ENOSPC in a job is a capacity result rather than a benchmark fluke. The published size was raised from 4 GiB to 10 GiB when the bench bake landed: the profile alone fills about 4.4 GiB, so a 7.06 GiB artifact would have started a baked job with barely 1.3 GiB free, which is where site creation and the test database run. A guest disk of 10240 MiB then proved too small for a completed job, which finished with about 1.4 GiB free and a Redis that began refusing writes; at 14336 MiB the same job finished with 5.5 GiB free. Raise the guest disk before raising the published size: the artifact is truncated to a target either way, and only the disk is what a running job actually gets.

An image published before this recipe stopped retaining the `wkhtmltox` archive still carries that 17 MB package under `/opt/runway`. Its raw filesystem SHA-256 is what identifies it, because the digest this recipe records was not in its manifest; the package inside it was confirmed separately, by extracting it from the published filesystem and matching the official release download. Build-specific measurements and runtime evidence belong with the pilot's recorded results, not in this recipe.

This recipe installs dependencies, and then the bench. `uv` is now baked rather than fetched per job: it is downloaded from its own release, checked against a digest pinned in the script, and cross-checked against the digest published beside that asset, so a republished binary stops the build the same way the Node digest does. What it does **not** bake is a database, a site, or any GitHub credential: a job creates its own site every run, and nothing here can know its database root password. The bake also refuses to touch a guest that has been registered or that already carries a manifest, and it will not build into a bench directory it cannot account for, because that residue belongs to a build that failed partway.

### Bake the bench

`guest/prepare-frappe-bench.sh` does the expensive part of a Frappe job once: it clones the pinned Frappe source, runs `bench init`, resolves the dev and Node dependencies, and builds production assets into `/home/runner/work/frappe/frappe`, which is the job's own `$GITHUB_WORKSPACE`. It records what it built in `/etc/runway-frappe-bench.json` and deletes the dependency caches afterwards, which is worth about 1.2 GiB and would otherwise ship in every clone. It is called by `prepare-frappe-image.sh`, or run on its own against a throwaway guest to test a change without publishing anything.

Two things about the bake are deliberate. The bench lives at the workspace path rather than a fixed directory, because a virtualenv carries absolute paths and a bench moved after the fact produces an interpreter that cannot import its own site-packages. And the app's remote is repointed at GitHub, because `bench init` leaves it pointing at the build-time staging directory, which the bake then deletes; a manifest check later refuses an image where any remote still names that path.

The bake also leaves a gzip-compressed copy at `/opt/runway/bench.tar.gz`, outside any
workspace, and records its digest and size in the manifest as an integer number of bytes. That is not
redundancy for its own sake: a job's own checkout clears its workspace, and a real job found the bench
gone by the time it looked while the manifest beside the image still claimed one. Nothing a job does
can reach the archive, so the setup action restores from it when the workspace has no bench, after
checking the digest against the manifest. An archive that does not match is a different artifact, and
restoring it would run code nobody vouched for.

The archive has to contain five directories, not just the source: `apps`, `sites`, `config`, `logs` and
`config/pids`. `bench` refuses to run in a directory missing any of them, and an archive built
without `logs` restored cleanly, passed the manifest's own checks, and still produced a bench that
every later command rejected with "Command not being executed in bench directory". A job then failed
at Create Site and the image looked healthy until a test ran. The bake now verifies the archive
against bench's own requirement rather than against the set of paths the bake happens to use, because
that distinction is what let an unusable bench through. Confirm it on a fresh guest before publishing
by listing those five paths out of the archive; the published image is the thing to check, not the
builder it came from.

The build also needs `/tmp` to be a real temporary directory. A guest whose `/tmp` was `ubuntu:ubuntu`
at mode 775 made every `apt` repository report "not signed", because apt drops to `_apt` to verify and
that account cannot write there. `1777 root:root` is the only correct answer and is worth setting
before the first `apt` call.

`scripts/validate_bench_manifest.py` decides whether a claimed bake is real, and `seal-runner-image.sh` runs it before publication. It checks the manifest's key set exactly, so an unexpected field cannot ride into a shared artifact, resolves the app's commit from the bench itself rather than believing the manifest, and compares the recorded sizes against the filesystem. That is deliberately not the recipe that wrote the manifest checking its own work: an image whose bench disagrees with its manifest would send every job down a fast path that tests code the image does not contain, which is a worse failure than having no bake at all.

After sealing and publishing, verify a fresh clone by reading the manifest, running the tool cache `python3.14` and `node` by path, `yarn --version`, `psql --version`, `mariadb --version`, and `wkhtmltopdf --version`, confirming both tool cache entries belong to the runner user, and confirming `.runner` and `.credentials` are still absent.

## Add the service container images

A Frappe job declares three services, so the runner pulls them on every job, on a fresh VM, over the region's path to the registry. Measured on the paired baseline, `Initialize containers` took 20 s on GitHub-hosted and 45 s on Runway. A local registry mirror only closed that to 42 s, because a mirror still hands the job compressed layers to decompress. Baking the layers into the image removes both, since by the time the job runs they are already uncompressed content in the guest's own store.

[`guest/prepare-frappe-container-images.sh`](../guest/prepare-frappe-container-images.sh) pulls them once, in the builder. Run it as root inside the same builder, after the guest installer and after the dependency profile, and before sealing:

```sh
bash prepare-frappe-container-images.sh --inside-unregistered-image-builder
bash seal-runner-image.sh --inside-unregistered-image-builder --preserve-container-images
```

The recipe pulls each image pinned by index digest and tags it with the reference the workflow actually requests, because a job pulls by tag and a digest-pinned pull alone would not leave the tag resolvable. It refuses a builder that is registered, that carries a JIT bootstrap marker, that has run containers or volumes, or that holds a Docker auth config: a credential in this guest would ship inside the published image, and deleting it afterwards cannot make a pull the credential authenticated un-credential-free. The flag is required rather than advisory, for the same reason sealing requires it.

Sealing then validates the result before anything is destroyed: the guest must be fresh, the manifest must parse and name exactly the three reviewed references, the store must hold exactly those three images and nothing else, each workflow tag must resolve to the content pinned for it, and no registry credential may remain. Only after all of that does it stop the daemons and freeze the store. The pinned list is repeated in the sealer on purpose. The manifest is written by the bake recipe, and the guard that decides what enters a public artifact must not be satisfied by the script that produced the content.

`--preserve-container-images` is opt-in. Without it the sealer stops Docker and removes `/var/lib/docker` and `/var/lib/containerd` exactly as before, and the resulting image has no container content at all.

### Where the images live

Docker 29 on Ubuntu 24.04 uses the containerd image store, so the image content is in `/var/lib/containerd` and `/var/lib/docker` holds little more than metadata. Measuring only the Docker data root reported 184 KB for 1.4 GB of images. The manifest therefore records both roots and a total, and both directories are preserved.

`/etc/runway-container-images.json` records what the run actually observed: the pinned reference, and the amd64 image id taken from the store rather than copied from anywhere else. It is credential-free by construction, because every value in it is a public image reference or a measurement of the builder's own filesystem.

### Size

Baked service images need more room than the 4 GiB default. The builder that produced this profile used a fresh 10 GiB guest, one vCPU and 2 GiB RAM, and measured 4.7 GiB used, so the export is published at `--size-gib 7`:

```sh
bash scripts/finalize-runner-image.sh --size-gib 7 /private/runner.ext4
```

`--size-gib` accepts 4 through 16 and defaults to 4, so the runner-only publish path is unchanged. A 10 GiB export publishing at 7 GiB is a shrink, and the finalizer resizes rather than refusing: it runs `resize2fs` first and lets that fail if the content does not fit, then confirms the filesystem is the target size before it truncates the file. Do not compare a builder's capacity against the published size yourself, because capacity is not content.

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

The finalizer repairs the filesystem before it inspects it. A stopped VM's export carries a dirty ext4 journal, and `debugfs` reads a filesystem without replaying one, so a file that is present can be reported as missing and an inode bitmap can fail its checksum for reasons that no credential problem explains. The finalizer therefore runs `e2fsck -f -p` first, accepts the statuses that mean repaired, and only then checks the sealing marker and the absent registration files.

`e2fsck -p` never answers yes on the operator's behalf and never makes a change it is unsure about. When it cannot finish, the finalizer stops and says so instead of inspecting an unrepaired export, because those file checks are the only credential guard a published image gets. Review the unmounted export yourself, repeat the finalizer on a clean result, and verify a fresh VM afterwards. An export that needed unreviewed repair is not evidence for anything.

The finalizer then resizes the filesystem to the requested size before truncation, replaces the ext4 journal, and zeroes unused blocks. The last two steps remove residual deleted identity material from free blocks and the old journal. It then writes a fresh compressed artifact and prints both hashes. Never run it against a mounted filesystem or a live VM disk. The Frappe profile image exposed this ordering: `e2fsck -f -p` exited 4 on the export, a manual `-f -y` pass repaired an orphaned inode, and the finalizer then completed and published. Treat that as the reason the repair comes first, not as a routine step.

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

### Why the host must retain the image, and how to check that it does

A fresh guest waits about 1.4 s for Metal to start the VM and then about 12 s to boot, but Metal's own start phase is not reliably 1.4 s. Across five consecutive starts it was 1.40, 1.40, 1.39, 37.12 and 37.76 s, so two of five paid an extra 37 s that nothing in the guest did.

That phase is the image being fetched. The path is `machine.Start`, which finds neither a disk nor a matching memory snapshot and takes `coldBoot`, which calls `relaunch`, which calls `prepareBoot`, which calls the storage layer's `PrepareBoot`, which calls `ensureImage`. `ensureImage` returns at once when the image is already stored on the host and complete, and otherwise downloads and extracts the rootfs, which is the 37 s. It is host work, so it happens before the guest exists and cannot be observed from inside it.

Whether the image is already stored is a policy decision, not a side effect. The host is told what to retain through an image policy that Atlas builds from every image with `cache_image` set, and `get_desired_images` selects on exactly that field. The default is unset, and this recipe's publisher never set it, so the policy on the Metal host is an empty list: nothing is meant to be retained, and the copy that happens to be on the host is there because something fetched it rather than because anything asked for it. That is not a durable state, and an operator pruning unused images is explicitly documented as safe when no VM uses them.

So the published image needs `cache_image` set on its record. With the current publisher that is a field on the image rather than an argument, so it has to be ticked on the record after publishing, and it has to be verified rather than assumed, because an image that looks published and is not in any host's policy fails open: the first job after a prune is slow and nothing reports an error.

This is worth doing: with the flag unset, two of five consecutive starts on one host paid an extra 37 s inside Metal's start phase, and with it set three consecutive starts were 1.37, 1.39 and 1.37 s. The measurement is in [the performance report](frappe-performance.md). Two checks, in order. Read the image record and confirm `cache_image` is set. Then read the host's `/var/lib/metal/image-policies.json` and confirm the image's reference appears in it, because that file is what Metal actually acts on and it is the only one of the two that proves the host was told.

### Ask for a warm artifact, and what it commits you to

Metal can keep a paused microVM of an image on each host and resume one on first boot, which replaces the guest kernel and init with a memory restore. That matters here because a fresh guest spends about 27 s booting before the runner starts, against about 1.4 s for Metal's own VM start and about 6 s connecting to GitHub, so boot is the largest identified part of what a job waits for. Pass the pool's exact shape to ask for it:

```python
publish_ubuntu_image(
    "Runway Ubuntu 24.04 runner 2.337.0 warm",
    "24.04",
    "amd64",
    Path("/private/runner.ext4"),
    Path("/private/vmlinux-5.10.223-fc-ci"),
    storage="Site File",
    memory_snapshot_shape=(3, 6144, 10240),
)
```

The three values are `(virtual_cpu_count, memory_mib, disk_mib)` and they are the pool's `cpu_millicores` divided by 1000, its `memory_mib`, and its `disk_mib`. Metal restores the artifact only on an **exact** match of all three, and only on a **first boot from a fresh disk clone**, which is what a per-job guest is. A shape recorded here that the pool does not request buys nothing and falls back to a cold boot without saying so, so the two have to be published together. The publisher also sets `cache_image`, because a host only builds an artifact for an image it is told to retain locally.

Two costs are worth weighing before publishing one:

- **The shape becomes fixed.** A warm artifact serves exactly one shape, so the pool cannot change `cpu_millicores`, `memory_mib` or `disk_mib` without a new image, because the field cannot change after creation and an existing warm guest is only valid at the shape it was captured at. - **The artifact is built lazily, per host.** Metal boots an isolated guest, waits about five minutes for it to settle, pauses it and promotes the result, so the first job after a publish pays for the build and later ones do not. A host that has never built it cold boots, again without saying so.

Verify rather than assume. Metal logs a warm start distinctly from a cold one, and the record's `memory_snapshot` and `memory_snapshot_configuration` fields are readable on the published image. Absence of a warm start in the log for a shape you published is the failure to look for, because every other signal reports success.

The publisher expects `runner.ext4.zst` beside the raw filesystem. Commit the transaction, record the new image ID and digests, and confirm it is Available and enabled. Use a distinct title when preserving an earlier image for rollback; publishing a changed image under the same title retires its predecessor.

Boot a second fresh VM from the published image. Without installing packages, check that the runner prints its expected version, Docker responds as the runner user, a published Docker Redis port returns `PONG`, and cloud-init generated fresh SSH host keys and machine identity. For an image published with a warm artifact, confirm from Metal's log that the boot resumed rather than cold booted, because every check below passes either way. Confirm that `.runner` and `.credentials` are absent before any JIT handoff. For an image that carries baked service images, also confirm each workflow tag resolves to its pinned digest and that `/etc/runway-container-images.json` is present with no `.docker` config anywhere. Delete this verification VM after validation. Retain its test evidence and the image's package manifest at `/etc/runway-runner-image.txt`.

The resulting image accelerates bootstrap. It does not implement GitHub-hosted image parity, automatic package updates, image signing, or an image promotion policy.
