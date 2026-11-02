# ADR 0003: Back-to-back conversion with one provider, simulated by Ditto

Status: accepted (2026-11-02)

**Context.** A non-bank FX payments business should not hold currency risk. The showcase needs a provider that fails on demand and keeps its own evidence.

**Decision.** Price off the provider's firm quote and book the trade with the provider when the customer pays, against prefunded balances. Ditto simulates the provider and the payout rail behind the same interface a real adapter would implement, and Corridor's provider contract suite certifies it.

**Consequences.** No open FX position; the spread is earned at booking; a fill difference is Corridor's own small gain or loss, booked to its own account. The trade is an ambiguous external call, resolved by reference and by a fence. Every failure is reproducible.

**Alternatives considered.** An internal FX desk with hedging (a treasury product, not an orchestration showcase); in-process mocks (no real HTTP, webhooks or statements).
