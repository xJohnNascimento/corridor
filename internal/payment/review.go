package payment

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/xJohnNascimento/corridor/internal/provider"
	"github.com/xJohnNascimento/corridor/internal/store"
)

// ApplyApproval applies an approved decision after checking the provider's evidence again. If the
// provider now contradicts it, the contradiction is recorded, nothing moves and the review stays
// open under a new revision, so a fresh proposal can follow.
func (a *Activities) ApplyApproval(ctx context.Context, paymentID string, approval Approval) (Applied, error) {
	t, err := a.terms(ctx, paymentID)
	if err != nil {
		return Applied{}, err
	}
	switch approval.Decision {
	case "ACCEPT_FILL":
		return a.acceptFill(ctx, t, approval)
	case "PAID":
		return a.resolvePaid(ctx, t, approval)
	case "NOT_PAID":
		return a.resolveNotPaid(ctx, t, approval)
	}
	return Applied{}, errors.New("decision " + approval.Decision + " is not built in this release")
}

func (a *Activities) acceptFill(ctx context.Context, t terms, approval Approval) (Applied, error) {
	trade, err := a.Provider.Trade(ctx, t.conversion.PaymentID.String())
	if err != nil {
		return Applied{}, providerError(err)
	}
	delivered, err := providerAmount(trade.BuyAmount)
	if err != nil || trade.Status != provider.TradeFilled || delivered != approval.Delivered {
		return Applied{}, a.contradicted(ctx, t, approval, "the provider's trade now reads "+trade.Status+" "+trade.BuyAmount)
	}
	entry, err := a.Ledger.CommitConversion(ctx, t.conversion, t.reservationID, approval.Delivered)
	if err != nil {
		return Applied{}, ledgerError(err)
	}
	id := t.conversion.PaymentID
	return Applied{Done: true, PaymentStatus: Converted, PayoutStatus: Planned}, a.Store.InTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "update fx.trade set entry_id = $2 where payment_id = $1", id, entry); err != nil {
			return err
		}
		if err := a.paymentStep(ctx, tx, id, paymentTransition("converted", []PaymentStatus{Converting}, Converted, "")); err != nil {
			return err
		}
		return resolved(ctx, tx, id, approval)
	})
}

func (a *Activities) resolvePaid(ctx context.Context, t terms, approval Approval) (Applied, error) {
	payout, err := a.Provider.Payout(ctx, t.payout.PayoutID.String())
	if err != nil && !errors.Is(err, provider.ErrNotVisible) {
		return Applied{}, providerError(err)
	}
	if payout.Status == provider.PayoutFailed || payout.Status == provider.PayoutCancelled {
		return Applied{}, a.contradicted(ctx, t, approval, "the provider now reads the payout "+payout.Status)
	}
	entry, err := a.Ledger.CommitPayout(ctx, t.payout, t.payment.Payout.ReservationID)
	if err != nil {
		return Applied{}, ledgerError(err)
	}
	return Applied{Done: true, PaymentStatus: Completed, PayoutStatus: PayoutCompleted}, a.finishPayout(ctx, t, approval, entry, approval.EvidenceID, Completed, PayoutCompleted, "")
}

// resolveNotPaid fences the payout at the provider first: only a CANCELLED or FAILED answer to the
// fence proves that nothing was paid.
func (a *Activities) resolveNotPaid(ctx context.Context, t terms, approval Approval) (Applied, error) {
	reference := t.payout.PayoutID.String()
	fence, err := a.Provider.CancelPayout(ctx, reference)
	if err != nil {
		return Applied{}, providerError(err)
	}
	if fence.Status != provider.PayoutCancelled && fence.Status != provider.PayoutFailed {
		return Applied{}, a.contradicted(ctx, t, approval, "the provider's fence answered "+fence.Status)
	}
	evidence, _, err := store.RecordEvidence(ctx, a.Store.Pool(), store.Evidence{Source: "FENCE", Operation: "PAYOUT", Reference: reference, Outcome: fence.Status, Payload: fence})
	if err != nil {
		return Applied{}, err
	}
	entry, err := a.Ledger.ReleasePayout(ctx, t.payout, t.payment.Payout.ReservationID)
	if err != nil {
		return Applied{}, ledgerError(err)
	}
	return Applied{Done: true, PaymentStatus: PayoutFailed, PayoutStatus: Failed}, a.finishPayout(ctx, t, approval, entry, evidence, PayoutFailed, Failed, "RESOLVED_NOT_PAID")
}

func (a *Activities) finishPayout(ctx context.Context, t terms, approval Approval, entry, evidence string, paymentStatus PaymentStatus, payoutStatus PayoutStatus, reason string) error {
	id := t.payout.PaymentID
	return a.Store.TransitionPayout(ctx, id, payoutTransition("resolved", []PayoutStatus{Submitting, Sent}, payoutStatus, "PAYOUT_"+string(payoutStatus)),
		func(ctx context.Context, tx pgx.Tx) error {
			returned := "null"
			if payoutStatus == Failed {
				returned = "amount"
			}
			if _, err := tx.Exec(ctx, "update fx.payout set evidence_id = $2, entry_id = $3, returned_amount = "+returned+" where payment_id = $1", id, evidence, entry); err != nil {
				return err
			}
			if err := a.paymentStep(ctx, tx, id, paymentTransition("resolved", []PaymentStatus{PayingOut}, paymentStatus, reason)); err != nil {
				return err
			}
			return resolved(ctx, tx, id, approval)
		})
}

// resolved closes the review and marks the approval applied with the effect it produced.
func resolved(ctx context.Context, tx pgx.Tx, paymentID uuid.UUID, approval Approval) error {
	if _, err := tx.Exec(ctx, `
		update fx.payment set review_open = false, review_reason = null, version = version + 1, updated_at = now() where id = $1`,
		paymentID,
	); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		update fx.product_command set state = 'APPLIED', applied_at = now(), effect_ids = array[$2] where id = $1 and state = 'ACCEPTED'`,
		approval.CommandID, approval.ProposalID,
	); err != nil {
		return err
	}
	return store.AddEvent(ctx, tx, paymentID, "REVIEW_RESOLVED", approval.Decision, "CUSTOMER", "resolved/"+approval.ProposalID)
}

func (a *Activities) contradicted(ctx context.Context, t terms, approval Approval, detail string) error {
	id := t.conversion.PaymentID
	return a.Store.InTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "update fx.payment set review_revision = review_revision + 1, review_opened_at = now() where id = $1", id); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "update fx.product_command set state = 'REJECTED', reason = 'evidence_contradicted' where id = $1", approval.CommandID); err != nil {
			return err
		}
		return store.AddEvent(ctx, tx, id, "APPROVAL_CONTRADICTED", detail, "OPERATOR", "contradicted/"+approval.ProposalID)
	})
}
