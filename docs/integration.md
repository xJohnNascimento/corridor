# Integration contract and proof

This document is the integration authority for the three repositories. Each service's API contract lives with its owner: the ledger's in the [ledger repository](https://github.com/xJohnNascimento/ledger/blob/main/docs/design.md#4-api), Ditto's in its [OpenAPI file and design](https://github.com/xJohnNascimento/ditto/blob/main/docs/design.md#3-api), Corridor's in [`design.md`](design.md#4-api). This one owns what crosses the boundaries: shared conventions, business references, the accounting of one payment, the evidence that allows money to move, the independent verifier and the scenarios. Freeze version 1 before the walking skeleton. Change it through a pull request here, together with a release of the ledger or Ditto when their contract changes.

Examples are synthetic. They describe intended behaviour, not measured results.

## 1. Identity, money and time

| Concern | Contract |
| --- | --- |
| Entity ids | UUIDv7 (RFC 9562) minted by the owning service. APIs show a readable prefix (`q_`, `pay_`, `po_`, `ben_`, `cmd_`); the prefix is presentation only and never encodes an amount or a permission |
| Dataset | Every local or sandbox installation has a dataset id. Evidence records it. A destructive reseed resets all three services together under a new id; idempotency records are never cleared selectively |
| Assets | `BRL`, `CAD`, `EUR` and `USD`, each with an immutable scale of 2. Adding an asset is configuration, not code |
| Amounts | Integer minor units. On the wire: a canonical decimal string (`"0"` or a nonzero digit followed by digits; signed fields allow one leading `-` on a nonzero value). Fractions, exponents, whitespace, leading zeros, `-0` and floats are refused |
| Amount ranges | The ledger stores signed 128-bit values and computes intermediates in arbitrary precision. Corridor and Ditto store `bigint` because a configured business limit (at most 250,000.00 per payment) keeps every product of amount and rate far inside 64 bits |
| Rates | Positive integers scaled by 10^8 (`rate_e8`), in bought units per sold unit. All rate arithmetic is integer with explicit rounding |
| Time | UTC instants. No timestamp is used to order commits across services |
| Customer identity | Taken from the authenticated credential, never from a body field |
| Canonical hash | SHA-256 over a versioned canonical object: operation, scope (ledger or customer), stable ids and normalized fields, with ordered journal lines kept in order. Field order in JSON never changes the hash; secrets and transport headers are excluded |
| Retention | References, tombstones, money facts and decision evidence live for the dataset's lifetime. Deduplication never expires |

## 2. Business references

Every external effect has a stable reference, persisted before the first attempt, and independent of the Temporal Activity attempt, the workflow run and the transport retry. `a / b` names a ledger pair (namespace `a`, reference `b`). Ditto references are plain ids, because its endpoints already say whether a reference is a quote, a trade or a payout.

| Fact | Owner and key | Durable identity |
| --- | --- | --- |
| Quote request | Corridor `(customer, Idempotency-Key)` | One provider quote request whose client reference is the quote id |
| Customer funds accounts | Ledger natural key `(corridor, CUSTOMER, <customer-id>, funds, <asset>, "")` | One per enabled currency, created at onboarding |
| Payment admission | Corridor `(customer, Idempotency-Key)`, one payment per quote | Payment id, a PLANNED payout with its id, and an immutable receipt, in one transaction |
| Workflow | Temporal workflow id `payment/<payment-id>` | Never reused automatically after it closes |
| Payout clearing account | Ledger natural key `(corridor, PAYMENT, <payment-id>, clearing, <bought asset>, "")` | One clearing account per payment |
| Conversion reservation | Ledger logical reference `corridor.conversion / <payment-id>`; command `corridor.reserve-conversion / <payment-id>/1` | Reservation id |
| Conversion settlement | Ledger command `corridor.commit-conversion / <payment-id>` | One journal entry with the conversion and any fill difference |
| Conversion release | Ledger command `corridor.release-conversion / <payment-id>` | Final reservation transition, no journal |
| Provider trade | Ditto trade reference: the payment id | One operation-reference row and one immutable request |
| Payout reservation | Ledger logical reference `corridor.payout / <payout-id>`; command `corridor.reserve-payout / <payout-id>/<n>` | One reservation; `n` starts at 1 and advances only after a durable refusal |
| Provider payout | Ditto payout reference: the payout id | One operation-reference row, fenceable before it exists |
| Payout settlement | Ledger command `corridor.commit-payout / <payout-id>` | One settlement journal |
| Payout return, reserved | Ledger command `corridor.release-payout / <payout-id>` | One release with the return journal |
| Payout return, never reserved | Ledger command `corridor.return-unreserved-payout / <payout-id>` | One direct return journal |
| Customer or operator command | Corridor `(actor, Idempotency-Key)` | Command id, also the Temporal Update id |
| Webhook | `(provider, event id)` | One inbox row; it may produce several Signals |
| Funding | Seed manifest entry | One Ditto funding movement and one ledger journal with the same reference |

Commit and release of the same reservation use different command references so they can race; the reservation row lock lets exactly one final state win. A caller that receives "already final" compares the final state and its fingerprint with its own intent: a matching state is success, any other state is a finding, never a silent success.

The two payout-return paths are exclusive. Corridor's cancellation gate and the durable outcome of the reserve command choose exactly one, and the verifier checks that a payout never has more than one return.

No network boundary offers exactly-once execution. Correctness comes from durable intent, retried delivery, receiver idempotency, serialized finalization and independently checked effects.

## 3. The worked example

CAD to USD, both scale 2. Acme sells CAD 10,000.00. Fee 50 bps, spread 5 bps, provider rate 0.73500000.

| Quantity | Formula | Minor units | Display |
| --- | --- | ---: | ---: |
| Sold | input | 1000000 | CAD 10,000.00 |
| Fee | `ceil(sell × 50 / 10000)` | 5000 | CAD 50.00 |
| Net sold | `sell − fee` | 995000 | CAD 9,950.00 |
| Provider rate | quote | 73500000 | 0.73500000 |
| Customer rate | `floor(lp_rate × 9995 / 10000)` | 73463250 | 0.73463250 |
| Provider delivers (quoted) | `floor(net × lp_rate / 10^8)` | 731325 | USD 7,313.25 |
| Customer receives | `floor(net × customer_rate / 10^8)` | 730959 | USD 7,309.59 |
| Spread | `lp_buy − buy` | 366 | USD 3.66 |

### Accounts

| Account | Normal side | Enforced | Owner |
| --- | --- | --- | --- |
| Customer funds, per currency | CREDIT | Yes | Customer |
| Provider cash, per currency | DEBIT | Yes | Provider account |
| Payment clearing, bought currency | CREDIT | Yes | One payment |
| Fee revenue, per currency | CREDIT | No | House |
| Spread revenue, per currency | CREDIT | No | House |
| FX execution difference, per currency | DEBIT | No | House (expense; a negative balance is a net gain) |
| Funding equity, per currency | CREDIT | No | House |

An enforced account must keep `available = posted − reserved + overdraft_limit ≥ 0` after every successful money command. For a credit-normal account a debit reduces funds; for a debit-normal account a credit does.

### Seed for this example

- Customer CAD: Dr provider cash CAD 1000000 / Cr customer funds CAD 1000000, mirrored by one Ditto funding movement.
- USD liquidity: Dr provider cash USD 100000 / Cr funding equity USD 100000, mirrored by one Ditto funding movement.

Seeds are explicit funding events, never unexplained opening balances. The demo and the scenarios use the larger seed manifest of [section 6](#6-integration-scenarios); the absolute balances below hold for a dataset seeded only as above.

### Conversion

Reserve, in one reservation: customer funds CAD 1000000 and provider cash CAD 995000. Settlement scope: those two, fee revenue CAD, provider cash USD, the payment's clearing USD, spread revenue USD and FX execution difference USD.

On a fill at the quoted amount, commit this one entry:

| Account | Debit | Credit |
| --- | ---: | ---: |
| Customer funds CAD | 1000000 | |
| Fee revenue CAD | | 5000 |
| Provider cash CAD | | 995000 |
| Provider cash USD | 731325 | |
| Payment clearing USD | | 730959 |
| Spread revenue USD | | 366 |

If the provider delivers 500 minor units less than quoted, the same entry gains two legs:

| Account | Debit | Credit |
| --- | ---: | ---: |
| FX execution difference USD | 500 | |
| Provider cash USD | | 500 |

The provider USD balance then rises by exactly 730825, what the provider delivered. A favourable difference uses the opposite sides. A zero difference adds no legs. Each currency balances on its own; the entry never balances a converted sum.

The execution difference records an observed trade cost or gain. It is not valuation, revaluation or statutory FX reporting, which belong to a general ledger.

### Payout

Reserve, in one reservation: payment clearing USD 730959 and provider cash USD 730959. Settlement scope: those two and customer funds USD.

| Outcome | Ledger effect |
| --- | --- |
| Paid | Commit: Dr payment clearing USD 730959 / Cr provider cash USD 730959 |
| Failed or cancelled after a reservation | Release, with this return journal in the same transaction: Dr payment clearing USD 730959 / Cr customer funds USD 730959 |
| Cancelled before any reservation | Direct return journal with the same legs, under the unreserved-return reference |

A cancelled or failed payout does not reverse the conversion: the CAD sale and the CAD fee stay booked, and the customer keeps the USD.

On a dataset seeded only as above, after a successful payout at the quoted fill, provider cash is CAD 5000 and USD 100366. With the adverse 500-unit fill it is CAD 5000 and USD 99866. Payment clearing is 0.

## 4. Evidence required before money moves

| Money action | Required durable evidence |
| --- | --- |
| Commit conversion | A filled trade matching currencies, sold amount and reference; an approved exact difference when it is beyond tolerance |
| Release conversion | A terminal rejection, or a closed (fenced) trade reference with no execution |
| Submit payout | An active payout reservation, the immutable payout request, a durable SUBMITTING gate and no accepted cancellation |
| Commit payout | A terminal COMPLETED read from the provider, or a terminal statement line, matching reference, amount and currency; an approval when it comes through review |
| Release payout after submission may have started | A terminal FAILED or CANCELLED for the reference, obtained by a read or a fence |
| Release payout before submission | A durable cancellation gate that excludes submission, and no submission marker |
| Apply an approval | Exact match of subject, review revision, proposal and evidence digest; distinct authenticated maker and checker; a durable accepted decision; an idempotent effect reference |

Never infer "no effect" from a timeout, a 404, a missed webhook, an expired credential or an empty Temporal query. When the required evidence is not available, keep the reservation and expose a retry or review state.

## 5. Independent verifier

`make verify` runs the checker in `verification/`, a separate Go module. It reads four sources:

1. The ledger's consistent audit export, read through a client with the `audit` scope: accounts, balances, postings, reservations and command outcomes.
2. Corridor's `ExportSnapshot`, read with the auditor key through the `fx_audit` role: payments, payouts, trades, evidence (with the operation it is about), ledger intents, commands and proposal records (maker, approver, digests).
3. Ditto's immutable statement and operation evidence.
4. Temporal descriptions of the known non-terminal payments, and visibility for investigation only.

The checker may share wire definitions, but it never calls the posting or pricing code it verifies. Its expectations come from an independent integer fold and hand-calculated fixtures.

Corridor's export is captured before and after the ledger's and Ditto's; if its records changed in between, the cut was not stable and the capture is repeated, up to three times, before the run gives up. Each report stores the source manifest hashes, capture times, schema versions, dataset and release ids, scope, observations and result. A missing export, a truncated page, an unsupported version or an unreachable source yields INCOMPLETE, never PASS. Temporal visibility alone never proves that a payment exists or is absent.

Corridor's scheduled reconciliation (v2, [design, section 11](design.md#11-reconciliation-and-watchdog)) will use the same checker module against live sources. In v1, `make scenarios VERIFY=1` runs the checker after every scenario.

### Invariants

| Id | Assertion |
| --- | --- |
| V1 | Every journal entry has 2 to 50 postings and balances per asset; every posting's account exists, belongs to the entry's ledger and has the posting's asset |
| V2 | Every posted balance equals the fold of its postings; each account's posting sequences run from 1 with no gap or duplicate |
| V3 | Every reserved balance equals the sum of its active reservation legs; a reservation is final exactly when it has a final command |
| V4 | No enforced account has negative availability: posted minus reserved plus overdraft limit |
| V5 | Every committed conversion has one matching provider fill and one settlement entry whose net provider-cash leg equals the delivered amount |
| V6 | Every completed payout has one matching COMPLETED provider outcome, one provider payment, one settlement entry and no return |
| V7 | Every failed payout, and every cancelled payout that was submitted, has a no-payment provider outcome (FAILED, CANCELLED or REJECTED) for its reference and terms; a cancelled payout never submitted has no provider operation; each has no provider debit, no unreleased reservation and one return crediting the paying customer's funds. The payout's submission time comes from the export; the paying customer is the owner of the funds account the payment's conversion debited |
| V8 | Every payment's clearing balance equals its bought amount while it is converted or paying out, and zero otherwise |
| V9 | Provider balances roll forward per currency, and each reference's provider net equals the ledger's provider-cash net unless its payment is still in flight |
| V10 | Every unfinished payment has a running workflow or a recovery review; a finished payment has no RECEIVED or ACCEPTED command and no unobserved ledger intent |
| V11 | Every approved proposal has distinct maker and approver, a matching digest, an approval command for its payment and unchanged cited evidence |

A cross-service gap is not automatically corruption. Live reconciliation matches by reference and classifies an external effect awaiting its ledger commit as IN_FLIGHT, within a policy age. A gap caused by separately captured snapshots requires a recapture before a mismatch is declared. A mature unmatched effect is a finding; it is never cleared by an aggregate adjustment.

For the deterministic demo, the runner stops new admissions and fault arming, waits for the expected workflow states, then captures the snapshots. It repeats the capture if identities or versions changed during it, and reports the stable cut it observed rather than claiming a distributed snapshot.

For live reconciliation, each run persists its source watermarks and overlaps the previous scan; it covers every unresolved item regardless of age and every item that became terminal since the last run. Findings carry forward by `(invariant, subject)`. Acknowledging a finding records who investigates it; resolving it requires new evidence. A FAIL report is kept; a rerun creates a new revision.

## 6. Integration scenarios

Each scenario runs against the real services and databases, triggers its failure at a named boundary, asserts what the customer sees, asserts provider and ledger counts and exact amounts, and then runs the verifier. The main suite runs scenarios one at a time for readability. A separate concurrency suite, built in v2, runs with unique references and deterministic barriers, repeatedly.

**Seed and isolation.** `make seed` loads `scenarios/seed.yaml`: the assets, the provider cash and funding-equity accounts (Corridor creates its revenue and difference accounts at boot), provider cash of 1,000,000.00 in each of BRL, CAD, EUR and USD (funded in Ditto and booked in the ledger with the same references), and the demo customers `acme` (50,000.00 in each currency) and `tinyco` (CAD 50.00). Each scenario creates its own customer, funded exactly for its payment, so no two scenarios share a customer balance. Provider cash is shared, so scenarios assert provider balance changes, not absolute balances; S14 and S15, which drain provider cash, run only in the serial suite and restore it afterwards.

v1 runs 23 scenarios: S01–S21, S24 and S25. S22, S23 and S26 arrive with v2 ([plan, M5](plan.md#m5-hardening-v2)).

| Id | Scenario | Trigger | Acceptance |
| --- | --- | --- | --- |
| S01 | Happy path | Default; once per enabled corridor | COMPLETED; for CAD→USD, the worked example's entries and balance changes to the minor unit; clearing 0 |
| S02 | Insufficient customer funds | A customer with CAD 50.00 sells CAD 1,000.00 | REJECTED (INSUFFICIENT_FUNDS); no reservation, no trade |
| S03 | Trade rejected | Ditto fault `TRADE_REJECT` | EXPIRED (RATE_UNAVAILABLE); conversion reservation released; no journal |
| S04 | Payout failed by the rail | Ditto fault `PAYOUT_FAIL` with code AC04 | PAYOUT_FAILED (BENEFICIARY_ACCOUNT_CLOSED); one return; USD back in the customer's balance |
| S05 | Cancel in the payout window | Customer cancel 5 s after CONVERTED | PAYOUT_CANCELLED; nothing submitted; one return |
| S06 | Same key, concurrent and changed | 3 concurrent CreatePayment calls with one key; then the key with another body | One payment and one workflow; identical receipts; the changed body is refused |
| S07 | API dies after admission | Failpoint after the admission commit; the client never retries | The dispatcher starts the workflow; the payment completes or ends EXPIRED; no orphan |
| S08 | Ledger commits, reply lost | Failpoint after the ledger commit | The same-reference retry returns the stored result; one journal |
| S09 | Trade fills, reply lost, worker dies | Ditto fault `TRADE_COMMIT_DROP_REPLY`; kill the worker | The lookup finds the fill; one trade, one conversion |
| S10 | Payout accepted, reply lost, worker dies | Ditto fault `PAYOUT_COMMIT_DROP_REPLY`; kill the worker | The lookup adopts the payout; one provider debit, one payment |
| S11 | Duplicate, reordered, omitted and false hints | Ditto faults `WEBHOOK_DUPLICATE`, `WEBHOOK_REORDER`, `WEBHOOK_OMIT`, `FALSE_FAILED_HINT` | No duplicate effect, no early release; polling and read-back converge |
| S12 | Cancel races submission | Cancel issued exactly at the release time, three times (the barrier-driven repetition belongs to the v2 concurrency suite) | Either an accepted cancel and zero submissions, or a refused cancel and a normal payout; never both |
| S13 | Late create races the fence | Ditto fault `DELAY_BEFORE_EXECUTION` holds every create past the quote, so Corridor closes the reference while the creates wait | The fence wins: the trade stays CLOSED, the payment expires and the customer's sell amount is released; Ditto's record shows the creates were held. Never a fill after the close, never a false no-payment proof |
| S14 | Provider liquidity wait and contention | Failpoint `payout.beforeReserve.hold` on two customers' converted payments; drain provider USD below the two payouts' total; release the hold; refund later | Provider cash is never overspent; the payouts that do not fit wait in WAITING_FUNDS with an alert; all complete after the refund |
| S15 | Cancel during the liquidity wait, reserve reply lost | Hold before the reservation and drain, so attempt 1 is refused (WAITING_FUNDS); refund; `ledger.afterCommit.holdReply` on attempt 2; customer cancel; release the reply | The reserve outcome is resolved first; exactly one return; no leaked reservation |
| S16 | Fill differences within tolerance | Ditto fault `FX_FILL_DIFFERENCE` −500 and +500 | One atomic entry each with the right difference legs; the customer amount unchanged |
| S17 | Fill beyond tolerance | `FX_FILL_DIFFERENCE` beyond 10 bps | Review; the exact fill plus two distinct operators settle it |
| S18 | Status API lags the statement | Ditto fault `STATUS_API_LAG` | PROCESSING alone settles nothing; PAID with the statement line and approval completes it |
| S19 | Payout stuck but cancellable | Ditto fault `PAYOUT_STUCK_CANCELLABLE` | Review; NOT_PAID approval plus the provider fence; one return |
| S20 | Wrong evidence, same person, superseded proposal | Proposals built to break each rule | Refused; reservations unchanged; every attempt recorded |
| S21 | Dispatcher dies after an accepted decision | Failpoint after ACCEPTED is stored | The decision is applied without a client retry |
| S22 | Ledger, provider or Temporal outage | Stop one dependency during 5 payments | Reads keep working where their database is up; nothing is released on a guess; all converge after recovery |
| S23 | Unexpected workflow termination | Terminate a payment workflow while its payout is SENT | Money stays reserved; the watchdog opens RECOVERY_REQUIRED; nothing reruns until two operators approve RESUME, after which the payment completes once |
| S24 | Checker catches bad data | The seeded fixtures of section 7 | The checker fails on the intended invariant |
| S25 | Incompatible workflow change | `make replay-break` | Replay fails without `GetVersion` and passes with it |
| S26 | Continue-As-New during review | A small history threshold; repeated commands across runs | Decisions, reservations and references survive; no duplicate action |

## 7. Checker-negative tests and release evidence

At least three corrupted snapshots must each fail a specific invariant: a missing fill-difference leg (V5), a second provider payout for one reference (V6), and a return next to a successful payout (V6, V7). An incomplete provider statement must yield INCOMPLETE. Corruption is applied to fixtures or disposable datasets; no production endpoint can mutate money data.

`make evidence`, built in v2 ([plan, P3](plan.md#m5-hardening-v2)), writes the release bundle:

```
release-manifest.json      commit, image digests, tool versions, dataset id
scenario-results.json      every scenario: PASS, FAIL or NOT RUN
verification-report.json   V1-V11 with source manifests
source-manifests/
temporal-histories/        sanitized
benchmark-results.json
environment.json
```

Tokens and synthetic beneficiary fields are removed before histories and logs are published. A test or deployment that was not executed is labelled NOT RUN, never "verified by design".

## 8. Claims this system does not make

- Exactly-once delivery across any network boundary.
- Survival of regional loss: the local and sandbox stacks run in one place.
- Immunity to forced workflow termination, history loss or arbitrary resets; those need a human with evidence (runbook).
- A wall-clock snapshot across services.
- Production readiness for real customer data; that needs compliance, a security review and field encryption first.
