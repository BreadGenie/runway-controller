# Development

Runway's CLI can be built and tested independently. A live execution needs an existing Central site, an Atlas region with a healthy Metal host, a compatible guest image, and GitHub access. The repository does not provision that environment end to end.

## Build and test

Use Go 1.26.3 or the version required by `go.mod`, Python 3 for script tests, and Bash for guest scripts.

```sh
go build -o runway-controller .
go test ./...
go vet ./...
python3 -m unittest discover -s guest -p '*_test.py'
python3 -m unittest discover -s examples -p '*_test.py'
python3 -m unittest discover -s scripts -p 'test_*.py'
bash -n guest/guest-runner.sh
bash -n guest/install-guest-runner.sh
bash -n scripts/build-ci-kernel.sh
./runway-controller --help
```

The CLI uses a pinned GitHub scale-set SDK dependency. Guest tools and VM helpers are separate from the CLI; running unit tests does not start a VM or modify GitHub.

## Prepare the infrastructure

Central and Atlas should run on separate Frappe sites. The inspected Central revision requires Python 3.14, Node.js 24, MariaDB 11.8, and Redis. Follow the [pinned Central setup guide](https://github.com/frappe/central/blob/6676d3178943d332c288fc55c04e6f44a8d01732/README.md) and verify requirements against the revisions you install.

Metal needs a Linux host with working KVM, supported kernel/networking features, ZFS, adequate image storage, and administrative access. Follow Atlas's [Generic provider guide](https://github.com/frappe/atlas/blob/339735de16471e90232cf8b1ff840696ee830ad9/docs/region/provider-guide.md) and [Metal development guide](https://github.com/frappe/atlas/blob/339735de16471e90232cf8b1ff840696ee830ad9/docs/develop/metal-testing.md). Do not treat the development host recipe as an untrusted-job isolation policy.

Before creating a runner, verify:

- Central signing keys and Atlas trust are initialized, the region connection test passes, and the intended Team has an allocated tenant identity.
- Generic host enrollment, host binaries, certificates, and fresh Metal usage synchronization are complete.
- Atlas can reach Metal's authenticated API, and Metal can download Atlas's image and binary artifacts. A local control plane may need SSH tunnels; endpoints and credentials are environment-specific.
- A compatible System image is available, schedulers and workers run, and Atlas's ten-second reconciliation jobs have an appropriate bench scheduler tick.

Complete the Atlas installation hooks and scheduler synchronization before using the region. Install the [Runway Frappe app](../integrations/central/README.md) on its own Frappe site. Configure its Central HTTPS origin and use one Central service user per Team.

## Prepare a compatible image

The tested baseline is Ubuntu 24.04 x86_64. Its guest kernel needs Firecracker's boot/storage/network support plus Docker bridge, overlay filesystem, raw netfilter, and CT support. See [results and pinned inputs](poc-results.md#artifact-provenance).

The kernel build recipe runs on Linux x86_64 and requires Bash, curl, tar, make, GCC, binutils, bc, flex, bison, OpenSSL headers, and ELF headers. Reserve several GiB of build space. It builds with three compiler jobs:

```sh
bash scripts/build-ci-kernel.sh /path/to/private/kernel-build
```

Publish the resulting kernel with a clean Ubuntu root filesystem through Atlas's image tooling. An image ID, artifact URL, or SSH key from another operator's environment is not portable. Keep image contents credential-free.

Create the guest through the operator helper before installing runner tools (substitute actual environment values):

```sh
/path/to/bench/env/bin/python scripts/atlas-vm.py create \
  --bench /path/to/bench --team TEAM --region REGION \
  --image-id IMAGE --ssh-public-key /path/to/guest.pub \
  --output /path/to/private/guest-create.jsonl
/path/to/bench/env/bin/python scripts/atlas-vm.py get \
  --bench /path/to/bench --team TEAM --region REGION --vm-id VM_ID
```

After creating a disposable guest, copy the scripts from `guest/` into it and execute the installer as root **inside that guest**:

```sh
bash install-guest-runner.sh --inside-disposable-guest
```

The installer prepares the official runner and guest-local Docker, checks kernel compatibility, and refuses to overwrite an existing runner installation. It does not install a daemon on Metal. The tested 4 GiB guest disk was tight; allow more space for larger workflows. Ubuntu packages are not pinned to a repository snapshot, so package versions can differ from the recorded experiment.

## Authenticate and register

Use a repository you administer and a dedicated runner label. The CLI privately reads the current `gh` credential by default. Alternatively provide all three GitHub App settings: `RUNWAY_GITHUB_APP_CLIENT_ID`, `RUNWAY_GITHUB_APP_INSTALLATION_ID`, and `RUNWAY_GITHUB_APP_PRIVATE_KEY_FILE`.

The App key stays on the operator/controller host. Only a fresh JIT configuration goes to the guest. Keep journals and JIT files outside tracked source, in a private directory:

```sh
mkdir -m 700 private
./runway-controller --action create --url https://github.com/OWNER/REPO --name runway-dogfood
./runway-controller --action jit --url https://github.com/OWNER/REPO \
  --id SCALE_SET_ID --name runway-guest-001 --out private/guest-001.jit
```

Creation modifies GitHub. Record the scale-set ID. If a response is lost, inspect remote state before repeating the operation. JIT output is an exclusive mode-0600 file; an empty file after failure is not a usable configuration.

Transfer the JIT file through the administrative channel, owned by the guest's unprivileged runner user with mode `0600`. As that user, start:

```sh
bash guest-runner.sh /home/runner/actions-runner /home/runner/guest-001.jit
```

The script consumes the transport file and supplies the JIT configuration through the runner's environment. Never snapshot or reuse a registered guest. Use a unique runner name for every attempt.

## Listen and run a workflow

Install [the smoke workflow](../examples/smoke-workflow.yml) in the selected repository and confirm its `runs-on` label matches the dedicated scale set. Start the listener, wait for `listening with capacity one; journal:` in its output, and only then manually dispatch that workflow. A job that finishes before the listener starts can be missed:

```sh
./runway-controller --action listen --url https://github.com/OWNER/REPO \
  --id SCALE_SET_ID --name runway-guest-001 \
  --journal private/guest-001.json --timeout 20m
```

Use exactly one guest and one journal for this scale set. On interruption, resume with the same journal. The local lock does not coordinate multiple machines or different journal paths. Live GitHub events may represent direct assignment with a zero request ID; the listener binds the stable job UUID to the expected runner and records completion before acknowledgment.


Confirm success independently in GitHub. Preserve a sanitized receipt and diagnostics. Listener timeout or process termination does not cancel the workflow or destroy the guest.

## VM operations and cleanup

Run `scripts/atlas-vm.py` with the Central bench's Python environment. Supply your bench, Team, Region, image ID, and guest SSH public key; inspect `--help` for the complete interface. These values deliberately have no deployment-specific defaults.

The helper creates through Central's Team-authorized Atlas client and reserves an exclusive creation journal before submitting one request. On an uncertain outcome, retain that journal and reconcile the attempt tag before trying another creation. The tool is an operator helper, not a lease controller or Central product flow.

After completion or failure, remove the GitHub scale set and terminate the guest as separate operations. Confirm the GitHub runner is gone and the tenant-scoped Atlas GET returns `404`. An accepted delete alone is insufficient.

```sh
./runway-controller --action delete --url https://github.com/OWNER/REPO --id SCALE_SET_ID
```

```sh
/path/to/bench/env/bin/python scripts/atlas-vm.py terminate \
  --bench /path/to/bench --team TEAM --region REGION --vm-id VM_ID
```

Keep credentials, JIT files, private keys, site configurations, image files, journals, and raw runner logs out of commits. The proof-of-concept environment and its credentials are not included in this repository.
