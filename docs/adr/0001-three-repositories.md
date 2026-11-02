# ADR 0001: Three repositories; Corridor integrates

Status: accepted (2026-11-02)

**Context.** The showcase has two products (the ledger and Corridor) and one simulator (Ditto). Each must be readable and runnable on its own, and the whole system must still start with one command for a reviewer.

**Decision.** Three repositories: `ledger`, `corridor` and `ditto`. Each owns its code, database, API contract, CI, release images and `infra/`. The ledger publishes its API as a separate, tagged Go module (`ledger/api`); Ditto publishes a versioned OpenAPI file. `corridor` is the integration point: its Compose stack pulls the `ledger` and `ditto` images pinned by digest, and it holds the integration contract, the scenarios, the independent verifier and the demo.

**Consequences.**

- Service ownership and versioned contracts are visible, as they are between real teams.
- A cross-repository change becomes a release followed by a version bump, which is slower than one pull request; `buf breaking` in the ledger and the provider contract suite in Corridor catch incompatible changes.
- Before the first releases exist, a Compose override builds the images from sibling checkouts.

**Alternatives considered.** One repository with three projects (one clone and atomic changes, but it hides service ownership and contract versioning); three repositories plus an umbrella repository for the integration (one more repository to explain, for no extra separation that matters here).
