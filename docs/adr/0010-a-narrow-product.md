# ADR 0010: A narrow product

Status: accepted (2026-11-02)

**Context.** The goal is to show engineering judgment and Temporal practice on a credible money flow, not every feature of a payments platform.

**Decision.** Build fixed-sell quotes for any enabled pair of BRL, CAD, EUR and USD (all with two decimals), one price schedule, back-to-back conversion, one payout per payment with a cancel window, liquidity waits, two-person resolution, reconciliation and the independent verifier. Compliance, fixed-buy amounts, requotes, returns, rail calendars, value dates, customer webhooks and real customer data stay as design notes.

**Consequences.** A reviewer can read the whole design and check every built piece through a test or a scenario. A real launch needs compliance first.

**Alternatives considered.** A full platform design (the earlier drafts ran to tens of thousands of words that no reviewer could verify).
