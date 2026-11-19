# runway-controller

Managed GitHub Actions runners for Frappe Cloud, with a path toward PR previews and deployments on push.

This repository is the Go side of Runway: the runner controller, the regional cache services, the guest agent and the image scripts. Runner pools, leases and Team connections live in the separate `runway` Frappe app, which runs on its own site and talks to Central over its API. The program is built as `runway-controller`.

Runway now includes a bounded internal pilot: a durable runner controller, the `runway` Frappe app for pools and leases, and a credential-free baked runner image recipe. Earlier execution experiments passed real GitHub workflows on fresh Atlas/Firecracker VMs, including Docker Redis services. A real workflow also passed after killing and restarting the durable controller; see the recorded evidence and limits.

The proposed product lets a team connect GitHub in Frappe Cloud and change `runs-on` in existing workflows. GitHub continues to own workflow execution, checks, logs, and artifacts. Frappe Cloud supplies runner compute and eventually connects those results to previews and deployments.

## What is here

- A controller using GitHub's scale-set client, with Central-backed admission, fenced ownership, durable leases, JIT bootstrap, and cleanup.
- A reviewable Central integration patch and operator tools for registration, one-job listening, and VM operations.
- Guest preparation and execution scripts, a Docker service-container smoke workflow, and a Docker-capable kernel recipe.
- Small VM lifecycle tooling and documentation of the tested infrastructure boundaries.

The pilot supports one concurrent runner per pool and a single-host administrative guest transport. It has no customer dashboard, billing integration, or validated isolation for untrusted jobs. A regional Actions cache runs only on the standalone guest path (see [regional cache](specs/runway-regional-cache.md)); the controller pilot still uses GitHub's cache. This repository does not install a working Frappe Cloud region by itself.

## Build

With Go 1.26.3 or newer:

```sh
go build -o runway-controller .
go test ./...
./runway-controller -h
```

See [development](docs/development.md) for the separate infrastructure and GitHub prerequisites needed for a live run.

## Start here

| Document | Purpose |
| --- | --- |
| [Internal pilot](docs/pilot.md) | Install, configure, operate, and recover the durable controller |
| [Image building](docs/image-building.md) | Bake and verify a clean runner image |
| [Architecture](docs/architecture.md) | Ownership and proposed integration boundaries |
| [Proof of concept results](docs/poc-results.md) | Live evidence, fixes, and limits |
| [Frappe performance](docs/frappe-performance.md) | Runner timing against GitHub-hosted, and what limits the test phase |
| [Development](docs/development.md) | Prerequisites and operator workflow |
| [Roadmap](docs/roadmap.md) | Milestones and decisions for the Frappe team |

The next milestone is a representative Frappe workflow and explicit network isolation, with broader failure drills for the internal pilot. The regional Actions cache is qualified on the standalone path; bringing it to the controller pilot and larger-scale rollout follow compatibility and reliability measurements.

See [LICENSE](LICENSE) and [NOTICE](NOTICE.md) for licensing and attribution.
