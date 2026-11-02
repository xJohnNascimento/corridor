# ADR 0002: Temporal runs the payment process

Status: accepted (2026-11-02)

**Context.** A payment waits from seconds to days, crosses three systems that can time out or lose answers, and sometimes needs people to decide.

**Decision.** Temporal runs each payment (`PaymentWorkflow`) and the scheduled reconciliation. Quotes, customer reads, the webhook inbox and command intake stay in fx-api and Postgres. The ledger uses plain database transactions.

**Consequences.** Durable timers, retries and human waits come without a hand-written job queue or scheduler. Reads keep working when workers are down. Workflow code must be deterministic, which `workflowcheck` and the replay gate enforce. Temporal is never the source of truth for money or for what customers read.

**Alternatives considered.** A database state machine with a job queue (timers, retries and recovery built by hand); a workflow per quote (history cost with no benefit).
