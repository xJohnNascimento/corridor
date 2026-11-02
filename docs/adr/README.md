# Architecture decisions

| ADR | Decision |
| --- | --- |
| [0001](0001-three-repositories.md) | Three repositories; Corridor integrates |
| [0002](0002-temporal-runs-the-payment-process.md) | Temporal runs the payment process; quotes, reads, inbox and commands stay in Postgres |
| [0003](0003-back-to-back-with-one-simulated-provider.md) | Back-to-back conversion with one provider, simulated by Ditto |
| [0004](0004-durable-admission-with-update-with-start.md) | Durable admission; Update-With-Start is the fast path, a dispatcher guarantees the start |
| [0005](0005-one-payment-workflow.md) | One payment workflow; the payout is a state machine, not a child workflow |
| [0006](0006-money-moves-only-on-durable-evidence.md) | Money moves only on durable evidence; absence is proven by a fence |
| [0007](0007-human-decisions-are-durable-commands.md) | Human decisions are durable commands delivered as Updates |
| [0008](0008-no-personal-data-in-workflow-history.md) | No personal data in workflow history |
| [0009](0009-versioning-with-getversion-and-a-replay-gate.md) | Versioning with `GetVersion` and a replay gate |
| [0010](0010-a-narrow-product.md) | A narrow product |
| [0011](0011-shared-foundation-through-ssm.md) | The shared cloud foundation lives here and is published through SSM |
| [0012](0012-stack.md) | Go, PostgreSQL, Connect and Temporal |
| [0013](0013-hand-written-sql-with-pgx.md) | Hand-written SQL with pgx in every service |

The ledger's own decisions (reservations, the single-writer design) are in the `ledger` repository's `docs/adr/`.

Format: context, decision, consequences, alternatives. A superseded ADR stays, marked with the ADR that replaces it.
