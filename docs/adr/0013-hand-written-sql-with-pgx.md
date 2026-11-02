# ADR 0013: Hand-written SQL with pgx

Status: accepted (2026-11-02)

**Context.** The plan first listed sqlc to generate typed query code. Once built, the three services' database work turned out to be of two kinds. Every money-moving write in the ledger is one call to a PL/pgSQL routine. Corridor's writes are compare-and-set transitions that lock a row, check its state and write events in one transaction. Ditto's are reference claims and leased outbox batches. Each is a handful of statements whose locking and ordering matter more than the mapping of columns to fields.

**Decision.** All three services use pgx v5 with hand-written SQL in their `store` packages, with each statement next to the transaction that needs it. Rows are mapped with `pgx.CollectRows` and explicit scans. Integration tests run every query against real PostgreSQL.

**Consequences.** The locking clauses (`FOR UPDATE`, `SKIP LOCKED`), the ordering and the transaction boundaries read in one place, in plain Go. There is no generation step to keep in sync. A typo in a query fails in a test rather than at compile time, which is why the stores are covered by integration tests.

**Alternatives considered.**

- sqlc: compile-time checked queries and generated types. It adds a generation step and a second place to read each query, for a small number of statements that mostly call routines or need dynamic locking.
- An ORM: hides the locking and transaction shape that the correctness arguments depend on.
