# ADR 0004: Durable admission; Update-With-Start is the fast path

Status: accepted (2026-11-02)

**Context.** The pay call must never create two payments for one request, never leave a payment without an owner, and should usually answer with the conversion already decided.

**Decision.** Admission first commits the payment row, its planned payout, its receipt and `workflow_start_pending`. The API then tries Update-With-Start as a fast path: the `awaitConversion` handler waits inside the workflow for the conversion decision, bounded by a 2 s workflow timer, and returns the status it reached. A dispatcher guarantees the start if the API crashes or Temporal is down. The answer is always the stored receipt plus the current snapshot.

**Consequences.** The first answer is usually CONVERTED. A Temporal outage does not fail admission. No "abandoned request" job is needed. The receipt is immutable while the snapshot changes. Two paths can start a workflow, and both use the same workflow id and conflict policy.

**Alternatives considered.** Update-With-Start alone (an API crash after the row needs a client retry or an expiry job); an asynchronous receipt alone (clients always poll for the decision).
