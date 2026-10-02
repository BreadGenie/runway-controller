# Runway

Managed GitHub Actions runners for Frappe Cloud, with a path toward PR previews and deployments on push.

Runway is currently an operator-run execution prototype. Two real GitHub workflows passed on fresh Atlas/Firecracker VMs, including Docker Redis services. A live scale-set message was redelivered, recorded durably, and acknowledged. Trial VMs, GitHub runners, and the temporary scale set were removed.

The proposed product lets a team connect GitHub in Frappe Cloud and change `runs-on` in existing workflows. GitHub continues to own workflow execution, checks, logs, and artifacts. Frappe Cloud supplies runner compute and eventually connects those results to previews and deployments.

## What is here

- An operator CLI using GitHub's scale-set client for registration, JIT configuration, one-job listening, and cleanup.
- Guest preparation and execution scripts, a Docker service-container smoke workflow, and a Docker-capable kernel recipe.
- Small VM lifecycle tooling and documentation of the tested infrastructure boundaries.

The prototype has no autoscaler, durable fleet controller, Central CI product model, billing integration, shared cache, or validated isolation for untrusted jobs. VM provisioning and teardown still require operator coordination. This repository does not install a working Frappe Cloud region by itself.

## Build

With Go 1.26.3 or newer:

```sh
go build -o runway .
go test ./...
./runway -h
```

See [development](docs/development.md) for the separate infrastructure and GitHub prerequisites needed for a live run.

## Start here

| Document | Purpose |
| --- | --- |
| [Architecture](docs/architecture.md) | Ownership and proposed integration boundaries |
| [Proof of concept results](docs/poc-results.md) | Live evidence, fixes, and limits |
| [Development](docs/development.md) | Prerequisites and operator workflow |
| [Roadmap](docs/roadmap.md) | Milestones and decisions for the Frappe team |

The next milestone is an internal dogfood runner pool operated through Central, with durable leases, bounded capacity, verified cleanup, and explicit network isolation. Caching and larger-scale rollout follow compatibility and reliability measurements.

See [LICENSE](LICENSE) and [NOTICE](NOTICE.md) for licensing and attribution.
