# ADR 0005: One payment workflow

Status: accepted (2026-11-02)

**Context.** Each payment has one conversion followed by one payout. The payout has a long tail: a cancel window, polling, liquidity waits and review.

**Decision.** One `PaymentWorkflow` per payment. The payout keeps its own table, status and transition table inside it. A child workflow is extracted only when payments gain several payouts, payouts become independently retried products, or measured histories justify it.

**Consequences.** One Update target and one history, bounded by tests and by Continue-As-New at safe boundaries. No child start, reset or parent-close cases to handle.

**Alternatives considered.** A parent with a child payout workflow (shows the pattern, but adds failure modes with no benefit at one payout per payment).
