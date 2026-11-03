# Runbooks

One section per review reason and per alert in [design, section 13.4](design.md#134-running-it). Commands assume the local stack (`docker compose exec fx-api /fx cli ...`, shortened here to `fx cli ...`).

Every resolution follows the same rules, enforced by the API and the workflow rather than by this page:

- A decision needs two operators: one proposes, another approves.
- A proposal binds the evidence it cites by digest. If the evidence or the proposal changes, the approval is refused.
- Applying a decision checks the provider again. A contradiction keeps the review open with a new revision; it never moves money.

## Reviews

### FILL_OUT_OF_TOLERANCE

**What happened.** The provider filled the trade at a buy amount further from the quote than `FillToleranceBps` allows, but not more than twice the quote; a fill beyond that opens PROVIDER_CONTRACT_BREACH instead, because no decision should book it. The customer's sell amount and the provider's cash are still reserved; nothing is committed.

**Decide.** Read the trade from the provider and compare its fill with the quote.

```bash
fx cli ops reviews --as alice
fx cli ops evidence read --payment PAYMENT_ID
fx cli ops propose --as alice --payment PAYMENT_ID --decision ACCEPT_FILL --evidence EVIDENCE_ID --digest DIGEST
fx cli ops approve --as bob --proposal PROPOSAL_ID --digest PROPOSAL_DIGEST
```

ACCEPT_FILL commits the conversion at the provider's fill and books the difference to `fx_difference`. Scenarios: S17, S20, S21.

### PAYOUT_UNCONFIRMED

**What happened.** The payout was sent, but the provider's status API has not confirmed an outcome within the polling budget. The clearing amount and the provider's cash stay reserved.

**Decide.** The statement is the provider's book of record; the status API can lag it.

| The statement shows | Decide | Effect |
| --- | --- | --- |
| A payout line for the payout id | PAID, citing the statement evidence | Commits the payout |
| No line, and the payout is still cancellable | NOT_PAID | Fences the payout at the provider, then returns the money to the customer |

```bash
fx cli ops evidence statement --payment PAYMENT_ID
fx cli ops propose --as alice --payment PAYMENT_ID --decision PAID --evidence EVIDENCE_ID --digest DIGEST
fx cli ops approve --as bob --proposal PROPOSAL_ID --digest PROPOSAL_DIGEST
```

If a NOT_PAID approval is refused with `evidence_contradicted`, the provider's fence found the payout already paid. Read the statement again and propose PAID. Scenarios: S18, S19.

### CONFLICTING_PROVIDER_EVIDENCE

**What happened.** A verified read of the payout disagrees with what Corridor sent: another amount, currency or reference. Corridor moved no money on it.

**Decide.** Capture a read and the statement. Escalate to the provider while they disagree with Corridor's records; once the statement settles it, resolve as for PAYOUT_UNCONFIRMED.

### PROVIDER_CONTRACT_BREACH, LEDGER_UNEXPECTED, RECOVERY_REQUIRED

A provider answer outside its contract, a ledger refusal that the design rules out, or a workflow that lost its run opens one of these reviews. The money stays reserved, and no v1 decision resolves them: RETRY_STEP and RESUME arrive with v2 ([plan, M5](plan.md#m5-hardening-v2)). In v1, read the payment's timeline and the activity's last failure in the Temporal UI, fix the cause, and keep the payment in review until v2's decisions apply.

## Alerts

The alerts are specified here and in the design; emitting them is part of the observability profile (C6). Until then, each condition can be checked by hand as described.

### Review older than 15 minutes

List open reviews with `fx cli ops reviews --as alice`, then follow the runbook for the reason. A review that waits has a cost but is safe: the money stays reserved.

### Money activity retrying past `AlertAfter`

A ledger or provider call that gates money has been failing for longer than `AlertAfter`. Money activities have no schedule-to-close, so they wait instead of failing a payment.

1. Find the activity in the Temporal UI (`localhost:8233`, search `PaymentStatus` and the pending activity) and read its last failure.
2. Check the dependency: `docker compose ps ledger ditto` and `docker compose logs --since 15m ledger ditto`.
3. When the dependency is back, the retry succeeds on its own. The stable reference makes the retried call idempotent, so no action is needed on the payment.

### Workflow task failures

A workflow task failing repeatedly is a code problem, most often a non-deterministic change. The workflow stops making progress but keeps its state.

1. Read the failure in the Temporal UI. A nondeterminism error names the command that changed.
2. Do not reset or terminate the workflow. Roll forward with a fix behind `workflow.GetVersion` ([ADR 0009](adr/0009-versioning-with-getversion-and-a-replay-gate.md)).
3. Add the failing history to `test/replay/` and run `make replay`, so the gate holds the fix.

### Inbox or dispatch backlog older than 5 minutes

The dispatcher has not delivered pending starts, commands or webhook hints.

1. `docker compose ps fx-dispatch` and `docker compose logs --since 15m fx-dispatch`.
2. Restart it with `docker compose restart fx-dispatch`. Its loops lease rows with `FOR UPDATE SKIP LOCKED`, so a restart or a second dispatcher never delivers twice.
3. If Temporal is down, the backlog drains when it returns. Payments already admitted are safe in PostgreSQL.

### Liquidity wait

A payment waits because the provider's cash in the bought currency is below what the payout needs. It retries on a durable timer and can still be cancelled.

1. Compare the ledger's provider cash with Ditto's balance (`make seed` prints both).
2. Fund the provider account at the provider (Ditto's `POST /admin/funding`), then post the same amount as a treasury journal from `funding_equity` to the provider's cash account in the ledger, so the two stay equal (V9). Waiting payments resume on their next attempt. Scenarios: S14, S15.

### Reconciliation FAIL or INCOMPLETE

Scheduled reconciliation arrives with v2. In v1, run `make verify`: a FAIL names the invariant and the subject, and an INCOMPLETE names the source that could not be read to its end.

### Bad deploy

Roll forward, never back: an older binary cannot replay histories written by a newer one. Ship the fix behind `workflow.GetVersion`, prove it with `make replay`, then deploy.
