# ADR 0009: Versioning with GetVersion and a replay gate

Status: accepted (2026-11-02)

**Context.** Payments in review can live for days, so code changes reach running workflows.

**Decision.** Every change to workflow commands goes behind `workflow.GetVersion`. A committed corpus of histories, captured from the scenarios, is replayed against every pull request; nondeterminism fails CI. `make replay-break` demonstrates both outcomes. Worker Deployments (pinning, auto-upgrade) are a later experiment.

**Consequences.** Incompatible changes are caught before merge. Old branches stay while any supported history can reach them.

**Alternatives considered.** Worker Deployments from day one (more moving parts for the first demo); no versioning (unsafe).
