# ADR 0012: Go, PostgreSQL, Connect and Temporal

Status: accepted (2026-11-02)

**Context.** Three small services must be correct with money, run on a laptop with one command, and be easy for a reviewer to read. The choices should be mainstream, well supported by Temporal, and the same across the three repositories.

**Decision.**

- **Go** for all three services: a mature Temporal SDK, static binaries in small images, and a strong concurrency and testing story.
- **PostgreSQL 17**, one database per service: transactions, row locks and constraints carry the ledger's guarantees, and the same engine runs locally and on Aurora.
- **Connect** (protobuf) for the ledger and Corridor APIs: one definition serves gRPC and plain HTTP/JSON, and `buf breaking` checks every change against the last release.
- **REST with OpenAPI** for Ditto, because most real providers expose REST, so Corridor's adapter shows a realistic integration.
- **Temporal** for orchestration ([ADR 0002](0002-temporal-runs-the-payment-process.md)); **Terraform** for infrastructure; **Docker Compose** locally.

**Consequences.** One language across the repositories. API contracts are generated and checked for breaking changes. Temporal's determinism rules in Go need lint (`workflowcheck`) and a replay gate.

**Alternatives considered.**

- Java or Kotlin with Spring: an equally mature Temporal SDK, but heavier for three small services.
- TypeScript: a good Temporal SDK, but weaker integer-money and database-locking ergonomics.
- A purpose-built ledger database such as TigerBeetle: much higher throughput, but it would hide the locking and invariant design this showcase exists to show.
- AWS Step Functions or a hand-written state machine instead of Temporal: vendor lock-in, or much more code.
- Plain gRPC (no curl-friendly JSON) or OpenAPI for the internal APIs (weaker breaking-change tooling than buf).
