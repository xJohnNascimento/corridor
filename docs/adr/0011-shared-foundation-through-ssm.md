# ADR 0011: The shared cloud foundation lives in Corridor and is published through SSM

Status: accepted (2026-11-02)

**Context.** With three repositories, each service deploys itself, but the network, the ECS cluster, service discovery and the GitHub OIDC provider are shared. Reading another repository's Terraform state couples the repositories through state layout and permissions.

**Decision.** `corridor/infra` creates the shared foundation and writes its outputs (VPC and subnet ids, cluster, discovery namespace, OIDC provider) to SSM Parameter Store under `/sandbox/foundation/`. Each repository's `infra/` reads those parameters, creates its own service, database, secrets and deploy role, and publishes its own endpoint under `/sandbox/<service>/`. Apply order: foundation, then `ledger` and `ditto`, then Corridor's services.

**Consequences.** No repository reads another's state; the SSM paths are the contract between them. Each service gets its own database cluster (database per service). The foundation must be applied first and destroyed last.

**Alternatives considered.** Each repository fully self-contained (its own network and cluster: more cost, and services need cross-network access); remote state data sources across repositories (tight coupling to state layout and access); a fourth platform repository (one more repository for a few resources).
