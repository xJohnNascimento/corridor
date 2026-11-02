package payment

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/xJohnNascimento/corridor/internal/failpoint"
)

// DecideCommand decides a command against durable state and stores the decision. It never waits
// on the workflow: it locks the payment and payout rows, in the same order as PrepareSubmit, and
// decides on the database clock. A decision already stored is returned as it is.
func (a *Activities) DecideCommand(ctx context.Context, paymentID, commandID string) (Decision, error) {
	d := Decision{CommandID: commandID}
	var operation string
	err := pgx.BeginFunc(ctx, a.Store.Pool(), func(tx pgx.Tx) error {
		var subject string
		err := tx.QueryRow(ctx, `
			select operation, coalesce(subject_id, ''), state, coalesce(reason, '') from fx.product_command where id = $1 for update`,
			commandID,
		).Scan(&operation, &subject, &d.State, &d.Reason)
		if err != nil {
			return err
		}
		if subject != paymentID {
			return errors.New("the command is for another payment")
		}
		if d.State != "RECEIVED" {
			d.Cancel = operation == "REQUEST_CANCEL" && d.State != "REJECTED"
			if operation == "APPROVE_RESOLUTION" && d.State == Accepted {
				d.Approval, err = approvalOf(ctx, tx, commandID)
			}
			return err
		}
		switch operation {
		case "REQUEST_CANCEL":
			return a.decideCancel(ctx, tx, paymentID, commandID, &d)
		case "APPROVE_RESOLUTION":
			return a.decideApproval(ctx, tx, paymentID, commandID, &d)
		}
		return errors.New("operation " + operation + " has no decision")
	})
	if err != nil {
		return Decision{}, err
	}
	if d.State == Accepted && a.Points.Hit(ctx, failpoint.CommandAfterAccepted, paymentID) {
		return Decision{}, failpoint.ErrInjected
	}
	return d, nil
}

// decideCancel applies the cancel window: a payout that has not reached its release time can be
// cancelled; one being submitted or already final cannot. A payout has one canonical cancellation,
// the first one accepted, so a later command can never take its place. The clock is read after
// the row locks are held, because a wait for them can outlast the release time.
func (a *Activities) decideCancel(ctx context.Context, tx pgx.Tx, paymentID, commandID string, d *Decision) error {
	var (
		status        PaymentStatus
		payout        PayoutStatus
		cancelCommand string
		beforeRelease bool
	)
	err := tx.QueryRow(ctx, `
		select p.status, o.status, coalesce(o.cancel_command_id::text, '')
		from fx.payment p join fx.payout o on o.payment_id = p.id
		where p.id = $1
		for update of p, o`,
		paymentID,
	).Scan(&status, &payout, &cancelCommand)
	if err != nil {
		return err
	}
	err = tx.QueryRow(ctx, "select coalesce(clock_timestamp() < release_at, true) from fx.payout where payment_id = $1", paymentID).Scan(&beforeRelease)
	if err != nil {
		return err
	}
	switch {
	case Terminal(status) || payout == Cancelling || payout == Cancelled || payout == Skipped:
		d.State, d.Reason = "REJECTED", "already_final"
	case cancelCommand != "":
		d.State, d.Reason = "REJECTED", "cancel_already_accepted"
	case payout == Planned || payout == Reserving || payout == WaitingFunds || payout == WaitingRelease && beforeRelease:
		d.State, d.Cancel = Accepted, true
	default:
		d.State, d.Reason = "REJECTED", "cancel_window_closed"
	}
	if _, err := tx.Exec(ctx, `
		update fx.product_command set state = $2, reason = nullif($3, ''), accepted_at = case when $2 = 'ACCEPTED' then now() end,
		       delivery_pending = false
		where id = $1`,
		commandID, d.State, d.Reason,
	); err != nil {
		return err
	}
	if d.State != Accepted {
		return nil
	}
	_, err = tx.Exec(ctx, "update fx.payout set cancel_accepted_at = now(), cancel_command_id = $2 where payment_id = $1", paymentID, commandID)
	return err
}

// decideApproval accepts an approval only for the exact current proposal of the open review, by an
// operator other than its maker. Anything else is refused and recorded with its reason.
func (a *Activities) decideApproval(ctx context.Context, tx pgx.Tx, paymentID, commandID string, d *Decision) error {
	var (
		approver, proposalID, digest string
		open                         bool
		reason                       string
		revision                     int
	)
	err := tx.QueryRow(ctx, `
		select c.actor_id, c.request->>'proposal_id', c.request->>'proposal_digest', p.review_open, coalesce(p.review_reason, ''), p.review_revision
		from fx.product_command c join fx.payment p on p.id = $2
		where c.id = $1
		for update of p`,
		commandID, paymentID,
	).Scan(&approver, &proposalID, &digest, &open, &reason, &revision)
	if err != nil {
		return err
	}
	var (
		maker, proposalReason, storedDigest string
		proposalRevision                    int
		superseded, approved                bool
	)
	proposal, err := uuid.Parse(proposalID)
	if err == nil {
		err = tx.QueryRow(ctx, `
			select maker, review_reason, review_revision, encode(digest, 'hex'), superseded_by is not null, approver is not null
			from fx.resolution_proposal where id = $1 and payment_id = $2
			for update`,
			proposal, paymentID,
		).Scan(&maker, &proposalReason, &proposalRevision, &storedDigest, &superseded, &approved)
	} else {
		err = pgx.ErrNoRows
	}
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		d.State, d.Reason = "REJECTED", "unknown_proposal"
	case err != nil:
		return err
	case storedDigest != digest:
		d.State, d.Reason = "REJECTED", "digest_mismatch"
	case superseded:
		d.State, d.Reason = "REJECTED", "proposal_superseded"
	case approved:
		d.State, d.Reason = "REJECTED", "already_approved"
	case maker == approver:
		d.State, d.Reason = "REJECTED", "maker_cannot_approve"
	case !open || proposalReason != reason || proposalRevision != revision:
		d.State, d.Reason = "REJECTED", "review_changed"
	default:
		d.State = Accepted
	}
	if _, err := tx.Exec(ctx, `
		update fx.product_command set state = $2, reason = nullif($3, ''), accepted_at = case when $2 = 'ACCEPTED' then now() end, delivery_pending = false
		where id = $1`,
		commandID, d.State, d.Reason,
	); err != nil || d.State != Accepted {
		return err
	}
	if _, err := tx.Exec(ctx, `
		update fx.resolution_proposal set approver = $2, approval_command_id = $3, approved_at = now() where id = $1`,
		proposal, approver, commandID,
	); err != nil {
		return err
	}
	d.Approval, err = approvalOf(ctx, tx, commandID)
	return err
}

func approvalOf(ctx context.Context, tx pgx.Tx, commandID string) (*Approval, error) {
	approval := &Approval{CommandID: commandID}
	err := tx.QueryRow(ctx, `
		select id::text, decision, coalesce(amount, 0), coalesce(evidence_id::text, '')
		from fx.resolution_proposal where approval_command_id = $1`,
		commandID,
	).Scan(&approval.ProposalID, &approval.Decision, &approval.Delivered, &approval.EvidenceID)
	return approval, err
}
