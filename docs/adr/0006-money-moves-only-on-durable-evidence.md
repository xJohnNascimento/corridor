# ADR 0006: Money moves only on durable evidence

Status: accepted (2026-11-02)

**Context.** A workflow's memory is replayed history. A timeout, a 404 or a missing webhook says nothing about what the provider actually did.

**Decision.** Every activity that commits, releases or submits acts only on durable evidence: a provider read stored once, a statement line, a fence result, or an accepted two-person decision. Absence is proven by a fence: closing a trade reference or cancelling a payout reference makes it unusable for good. Webhooks are hints and are always read back. CHECK constraints on `trade` and `payout` restate the evidence rules.

**Consequences.** Unknown outcomes keep money reserved and go to retry or review. The provider must offer lookup and fencing, which the contract suite certifies. Nothing is decided by elapsed time.

**Alternatives considered.** Inferring absence from time or a 404 (risk of paying twice); trusting webhook payloads (forgeable and reorderable).
