package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/xJohnNascimento/corridor/internal/provider"
)

// ReviewReason is why a payment is in review. It is defined here, below the payment package that
// re-exports it, because the store checks proposals against it and cannot import that package.
type ReviewReason string

const (
	ReviewFillOutOfTolerance          ReviewReason = "FILL_OUT_OF_TOLERANCE"
	ReviewPayoutUnconfirmed           ReviewReason = "PAYOUT_UNCONFIRMED"
	ReviewConflictingProviderEvidence ReviewReason = "CONFLICTING_PROVIDER_EVIDENCE"
	ReviewProviderContractBreach      ReviewReason = "PROVIDER_CONTRACT_BREACH"
	ReviewLedgerUnexpected            ReviewReason = "LEDGER_UNEXPECTED"
	ReviewRecoveryRequired            ReviewReason = "RECOVERY_REQUIRED"
)

// decisionsByReason lists the decisions each review reason accepts in this release.
var decisionsByReason = map[ReviewReason][]string{
	ReviewFillOutOfTolerance:          {"ACCEPT_FILL"},
	ReviewPayoutUnconfirmed:           {"PAID", "NOT_PAID"},
	ReviewConflictingProviderEvidence: {"PAID", "NOT_PAID"},
}

type Review struct {
	PaymentID         uuid.UUID
	Reason            string
	Revision          int32
	OpenedAt          time.Time
	CurrentProposalID string
}

func (s *Store) Reviews(ctx context.Context, limit int) ([]Review, error) {
	rows, err := s.pool.Query(ctx, `
		select p.id, p.review_reason, p.review_revision, p.review_opened_at,
		       coalesce((select r.id::text from fx.resolution_proposal r
		                 where r.payment_id = p.id and r.review_revision = p.review_revision and r.superseded_by is null
		                 order by r.proposal_revision desc limit 1), '')
		from fx.payment p
		where p.review_open
		order by p.review_opened_at
		limit $1`,
		limit,
	)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Review, error) {
		var r Review
		return r, row.Scan(&r.PaymentID, &r.Reason, &r.Revision, &r.OpenedAt, &r.CurrentProposalID)
	})
}

type Proposal struct {
	PaymentID         uuid.UUID `json:"payment_id"`
	SubjectKind       string    `json:"subject_kind"`
	SubjectID         string    `json:"subject_id"`
	ReviewReason      string    `json:"review_reason"`
	ReviewRevision    int       `json:"review_revision"`
	Decision          string    `json:"decision"`
	ProviderReference string    `json:"provider_reference"`
	Currency          string    `json:"currency"`
	Amount            int64     `json:"amount"`
	EvidenceID        string    `json:"evidence_id"`
	EvidenceDigest    string    `json:"evidence_digest"`
	Maker             string    `json:"maker"`
	ProposalRevision  int       `json:"proposal_revision"`
}

// digest binds every field of the proposal, so an approval names exactly what it authorizes.
func (p Proposal) digest() [sha256.Size]byte {
	encoded, _ := json.Marshal(p) // strings and integers only: it cannot fail
	return sha256.Sum256(encoded)
}

type RefusedError struct {
	Reason string
}

func (e *RefusedError) Error() string {
	return "refused: " + e.Reason
}

func refused(reason string) error {
	return &RefusedError{Reason: reason}
}

// openReview is the state of a payment's review and payout that a proposal is checked against.
type openReview struct {
	open     bool
	reason   ReviewReason
	revision int
	payout   string
	currency string
	amount   int64
}

// Propose checks a proposal against the open review and its evidence, supersedes any earlier
// proposal for the same review revision and stores it, and settles the proposing command in the
// same transaction: APPLIED with the proposal's id, or REJECTED with the refusal's reason. A
// refusal is also returned as RefusedError. A command that is no longer RECEIVED is left alone,
// so a retry of the same key never stores a second proposal.
func (s *Store) Propose(ctx context.Context, command uuid.UUID, p Proposal) error {
	id, err := uuid.NewV7()
	if err != nil {
		return err
	}
	var refusal *RefusedError
	err = s.InTx(ctx, func(tx pgx.Tx) error {
		var state string
		if err := tx.QueryRow(ctx, "select state from fx.product_command where id = $1 for update", command).Scan(&state); err != nil {
			return err
		}
		if state != "RECEIVED" {
			return nil
		}
		err := insertProposal(ctx, tx, id, p)
		if errors.As(err, &refusal) {
			return settleIn(ctx, tx, command, "REJECTED", refusal.Reason)
		}
		if err != nil {
			return err
		}
		return applyIn(ctx, tx, command, id.String())
	})
	if err != nil {
		return err
	}
	if refusal != nil {
		return refusal
	}
	return nil
}

func insertProposal(ctx context.Context, tx pgx.Tx, id uuid.UUID, p Proposal) error {
	var r openReview
	err := tx.QueryRow(ctx, `
		select p.review_open, coalesce(p.review_reason, ''), p.review_revision, o.id::text, o.currency, o.amount
		from fx.payment p join fx.payout o on o.payment_id = p.id where p.id = $1 for update of p`,
		p.PaymentID,
	).Scan(&r.open, &r.reason, &r.revision, &r.payout, &r.currency, &r.amount)
	if errors.Is(err, pgx.ErrNoRows) {
		return refused("unknown_payment")
	}
	if err != nil {
		return err
	}
	if err := checkProposal(ctx, tx, p, r); err != nil {
		return err
	}
	var evidenceID, evidenceDigest any
	if p.EvidenceID != "" {
		raw, err := hex.DecodeString(p.EvidenceDigest)
		if err != nil {
			return refused("evidence_digest_invalid")
		}
		evidenceID, evidenceDigest = p.EvidenceID, raw
	}
	err = tx.QueryRow(ctx, `
		select coalesce(max(proposal_revision), 0) + 1 from fx.resolution_proposal where payment_id = $1 and review_revision = $2`,
		p.PaymentID, p.ReviewRevision,
	).Scan(&p.ProposalRevision)
	if err != nil {
		return err
	}
	p.ReviewReason = string(r.reason)
	digest := p.digest()
	if _, err := tx.Exec(ctx, `
		insert into fx.resolution_proposal (id, payment_id, review_reason, review_revision, decision, subject_kind, subject_id,
		                                    provider_reference, currency, amount, evidence_id, evidence_digest, digest, maker, proposal_revision)
		values ($1, $2, $3, $4, $5, $6, $7, $8, nullif($9, ''), nullif($10::bigint, 0), $11, $12, $13, $14, $15)`,
		id, p.PaymentID, r.reason, p.ReviewRevision, p.Decision, p.SubjectKind, p.SubjectID, p.ProviderReference,
		p.Currency, p.Amount, evidenceID, evidenceDigest, digest[:], p.Maker, p.ProposalRevision,
	); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
		update fx.resolution_proposal set superseded_by = $1
		where payment_id = $2 and review_revision = $3 and id <> $1 and superseded_by is null and approver is null`,
		id, p.PaymentID, p.ReviewRevision,
	)
	return err
}

func checkProposal(ctx context.Context, q Querier, p Proposal, r openReview) error {
	switch {
	case !r.open:
		return refused("no_open_review")
	case p.ReviewRevision != r.revision:
		return refused("stale_review_revision")
	case !slices.Contains(decisionsByReason[r.reason], p.Decision):
		return refused("decision_not_allowed_for_" + string(r.reason))
	}
	if p.Decision == "ACCEPT_FILL" {
		if p.SubjectKind != "TRADE" || p.SubjectID != p.PaymentID.String() || p.ProviderReference != p.SubjectID {
			return refused("wrong_subject")
		}
		return checkEvidence(ctx, q, p, "TRADE", provider.TradeFilled)
	}
	if p.SubjectKind != "PAYOUT" || p.SubjectID != r.payout || p.ProviderReference != r.payout {
		return refused("wrong_subject")
	}
	if p.Decision == "NOT_PAID" {
		return nil
	}
	if p.Currency != r.currency || p.Amount != r.amount {
		return refused("amount_mismatch")
	}
	return checkEvidence(ctx, q, p, "PAYOUT", provider.PayoutCompleted)
}

// checkEvidence requires stored provider evidence for the same reference, with the outcome the
// decision needs, the proposed amount and the digest the operator saw.
func checkEvidence(ctx context.Context, q Querier, p Proposal, operation, outcome string) error {
	if p.EvidenceID == "" {
		return refused("evidence_required")
	}
	id, err := uuid.Parse(p.EvidenceID)
	if err != nil {
		return refused("unknown_evidence")
	}
	var (
		reference, got, currency, digest string
		amount                           int64
	)
	err = q.QueryRow(ctx, `
		select reference, outcome, coalesce(currency, ''), coalesce(amount, 0), encode(digest, 'hex')
		from fx.provider_evidence where id = $1 and operation = $2 and source in ('READ', 'STATEMENT')`,
		id, operation,
	).Scan(&reference, &got, &currency, &amount, &digest)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return refused("unknown_evidence")
	case err != nil:
		return err
	case digest != p.EvidenceDigest:
		return refused("evidence_digest_mismatch")
	case reference != p.ProviderReference || got != outcome || amount != p.Amount || currency != p.Currency:
		return refused("evidence_does_not_match")
	}
	return nil
}

// ReviewSubject is what a proposal needs to know about a payment and its payout.
type ReviewSubject struct {
	Reason   ReviewReason
	Revision int32
	PayoutID uuid.UUID
	Currency string
	Amount   int64
}

func (s *Store) ReviewSubject(ctx context.Context, payment uuid.UUID) (ReviewSubject, error) {
	var r ReviewSubject
	err := s.pool.QueryRow(ctx, `
		select coalesce(p.review_reason, ''), p.review_revision, o.id, o.currency, o.amount
		from fx.payment p join fx.payout o on o.payment_id = p.id where p.id = $1`,
		payment,
	).Scan(&r.Reason, &r.Revision, &r.PayoutID, &r.Currency, &r.Amount)
	if errors.Is(err, pgx.ErrNoRows) {
		return ReviewSubject{}, ErrNotFound
	}
	return r, err
}
