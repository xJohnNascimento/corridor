package store

import (
	"context"
	"errors"
	"math"
	"sync"
	"testing"

	"github.com/google/uuid"
)

func TestCheckProposalAppliesTheReviewRules(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := context.Background()
	payment := uuid.New()
	payout := uuid.NewString()
	evidenceID, evidenceDigest, err := RecordEvidence(ctx, s.Pool(), Evidence{
		Source: "READ", Operation: "PAYOUT", Reference: payout, Outcome: "COMPLETED", Currency: "USD", Amount: 731325, Payload: map[string]string{"status": "COMPLETED"},
	})
	if err != nil {
		t.Fatal(err)
	}
	review := openReview{open: true, reason: "PAYOUT_UNCONFIRMED", revision: 2, payout: payout, currency: "USD", amount: 731325}
	paid := Proposal{
		PaymentID: payment, SubjectKind: "PAYOUT", SubjectID: payout, ReviewRevision: 2, Decision: "PAID", ProviderReference: payout,
		Currency: "USD", Amount: 731325, EvidenceID: evidenceID, EvidenceDigest: evidenceDigest,
	}
	change := func(f func(*Proposal)) Proposal {
		p := paid
		f(&p)
		return p
	}
	cases := map[string]struct {
		review openReview
		p      Proposal
		want   string
	}{
		"a proposal that meets every rule": {review, paid, ""},
		"a payout not paid needs no evidence": {review, change(func(p *Proposal) {
			p.Decision, p.EvidenceID, p.EvidenceDigest, p.Currency, p.Amount = "NOT_PAID", "", "", "", 0
		}), ""},
		"no open review":                        {openReview{reason: "PAYOUT_UNCONFIRMED", revision: 2}, paid, "no_open_review"},
		"a stale revision":                      {review, change(func(p *Proposal) { p.ReviewRevision = 1 }), "stale_review_revision"},
		"a decision the reason does not accept": {review, change(func(p *Proposal) { p.Decision = "ACCEPT_FILL" }), "decision_not_allowed_for_PAYOUT_UNCONFIRMED"},
		"another payout":                        {review, change(func(p *Proposal) { p.SubjectID = uuid.NewString() }), "wrong_subject"},
		"another provider reference":            {review, change(func(p *Proposal) { p.ProviderReference = uuid.NewString() }), "wrong_subject"},
		"a trade subject for a payout":          {review, change(func(p *Proposal) { p.SubjectKind = "TRADE" }), "wrong_subject"},
		"another amount":                        {review, change(func(p *Proposal) { p.Amount = 1 }), "amount_mismatch"},
		"another currency":                      {review, change(func(p *Proposal) { p.Currency = "CAD" }), "amount_mismatch"},
		"no evidence":                           {review, change(func(p *Proposal) { p.EvidenceID = "" }), "evidence_required"},
		"a fill outside a fill review": {
			openReview{open: true, reason: "FILL_OUT_OF_TOLERANCE", revision: 2, payout: payout},
			Proposal{PaymentID: payment, SubjectKind: "PAYOUT", SubjectID: payment.String(), ReviewRevision: 2, Decision: "ACCEPT_FILL", ProviderReference: payment.String()},
			"wrong_subject",
		},
		"a fill without evidence": {
			openReview{open: true, reason: "FILL_OUT_OF_TOLERANCE", revision: 2, payout: payout},
			Proposal{PaymentID: payment, SubjectKind: "TRADE", SubjectID: payment.String(), ReviewRevision: 2, Decision: "ACCEPT_FILL", ProviderReference: payment.String()},
			"evidence_required",
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			requireRefusal(t, checkProposal(ctx, s.Pool(), c.p, c.review), c.want)
		})
	}
}

func TestCheckEvidenceRequiresStoredEvidenceThatMatchesTheProposal(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := context.Background()
	reference := uuid.NewString()
	record := func(source, operation, outcome string, amount int64) (string, string) {
		id, digest, err := RecordEvidence(ctx, s.Pool(), Evidence{
			Source: source, Operation: operation, Reference: reference, Outcome: outcome, Currency: "USD", Amount: amount,
			Payload: map[string]string{"source": source, "operation": operation, "outcome": outcome},
		})
		if err != nil {
			t.Fatal(err)
		}
		return id, digest
	}
	id, digest := record("READ", "PAYOUT", "COMPLETED", 500)
	statementID, statementDigest := record("STATEMENT", "PAYOUT", "COMPLETED", 500)
	hintID, hintDigest := record("HINT", "PAYOUT", "COMPLETED", 500)
	fenceID, fenceDigest := record("FENCE", "PAYOUT", "COMPLETED", 500)
	failedID, failedDigest := record("READ", "PAYOUT", "FAILED", 500)
	tradeID, tradeDigest := record("READ", "TRADE", "FILLED", 500)

	proposal := func(id, digest string) Proposal {
		return Proposal{EvidenceID: id, EvidenceDigest: digest, ProviderReference: reference, Currency: "USD", Amount: 500}
	}
	cases := map[string]struct {
		p                  Proposal
		operation, outcome string
		want               string
	}{
		"a provider read":                   {proposal(id, digest), "PAYOUT", "COMPLETED", ""},
		"a statement line":                  {proposal(statementID, statementDigest), "PAYOUT", "COMPLETED", ""},
		"a trade read":                      {proposal(tradeID, tradeDigest), "TRADE", "FILLED", ""},
		"none cited":                        {proposal("", ""), "PAYOUT", "COMPLETED", "evidence_required"},
		"an id that is not a uuid":          {proposal("nope", digest), "PAYOUT", "COMPLETED", "unknown_evidence"},
		"an id never stored":                {proposal(uuid.NewString(), digest), "PAYOUT", "COMPLETED", "unknown_evidence"},
		"a webhook hint":                    {proposal(hintID, hintDigest), "PAYOUT", "COMPLETED", "unknown_evidence"},
		"a fence answer":                    {proposal(fenceID, fenceDigest), "PAYOUT", "COMPLETED", "unknown_evidence"},
		"evidence of another operation":     {proposal(tradeID, tradeDigest), "PAYOUT", "COMPLETED", "unknown_evidence"},
		"a digest the operator did not see": {proposal(id, tradeDigest), "PAYOUT", "COMPLETED", "evidence_digest_mismatch"},
		"another outcome":                   {proposal(failedID, failedDigest), "PAYOUT", "COMPLETED", "evidence_does_not_match"},
		"another reference": {func() Proposal {
			p := proposal(id, digest)
			p.ProviderReference = uuid.NewString()
			return p
		}(), "PAYOUT", "COMPLETED", "evidence_does_not_match"},
		"another amount": {func() Proposal {
			p := proposal(id, digest)
			p.Amount = 501
			return p
		}(), "PAYOUT", "COMPLETED", "evidence_does_not_match"},
		"another currency": {func() Proposal {
			p := proposal(id, digest)
			p.Currency = "CAD"
			return p
		}(), "PAYOUT", "COMPLETED", "evidence_does_not_match"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			requireRefusal(t, checkEvidence(ctx, s.Pool(), c.p, c.operation, c.outcome), c.want)
		})
	}
}

func requireRefusal(t *testing.T, err error, want string) {
	t.Helper()
	var refusal *RefusedError
	switch {
	case want == "" && err != nil:
		t.Fatalf("refused: %v", err)
	case want != "" && !errors.As(err, &refusal):
		t.Fatalf("got %v, want a refusal for %s", err, want)
	case want != "" && refusal.Reason != want:
		t.Fatalf("refused for %s, want %s", refusal.Reason, want)
	}
}

func TestEvidenceIsStoredOnceAndKeptWhenItsBodyChanges(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := context.Background()
	record := func(sequence int, status string) (string, string) {
		id, digest, err := RecordEvidence(ctx, s.Pool(), Evidence{
			Source: "READ", Operation: "PAYOUT", Reference: "o-1", Outcome: "COMPLETED", Sequence: sequence, Payload: map[string]string{"status": status},
		})
		if err != nil {
			t.Fatal(err)
		}
		return id, digest
	}
	first, firstDigest := record(3, "COMPLETED")
	again, againDigest := record(3, "COMPLETED")
	if first != again || firstDigest != againDigest {
		t.Fatalf("the same observation was stored twice: %s and %s", first, again)
	}
	if other, _ := record(3, "COMPLETED-WITH-A-NEW-BODY"); other == first {
		t.Fatal("a changed body replaced the first observation")
	}
	if next, _ := record(4, "COMPLETED"); next == first {
		t.Fatal("a later sequence collapsed into an earlier one")
	}
}

func TestProposeSettlesItsCommandInTheTransactionThatStoresTheProposal(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := context.Background()
	p := admitted(t, s, "k-1")
	if _, err := s.Pool().Exec(ctx, `
		update fx.payment set review_open = true, review_reason = 'PAYOUT_UNCONFIRMED', review_revision = 1, review_opened_at = now() where id = $1`, p.ID); err != nil {
		t.Fatal(err)
	}
	payout := p.Payout.ID.String()
	proposal := func(revision int) Proposal {
		return Proposal{PaymentID: p.ID, SubjectKind: "PAYOUT", SubjectID: payout, ReviewRevision: revision, Decision: "NOT_PAID", ProviderReference: payout, Maker: "alice"}
	}
	claim := func(key string) Command {
		cmd, _, err := s.ClaimCommand(ctx, CommandClaim{ActorKind: "OPERATOR", ActorID: "alice", RequestKey: key, Operation: "PROPOSE_RESOLUTION", SubjectID: p.ID.String(), Request: key}, false)
		if err != nil {
			t.Fatal(err)
		}
		return cmd
	}
	proposals := func() int {
		var n int
		if err := s.Pool().QueryRow(ctx, "select count(*) from fx.resolution_proposal where payment_id = $1", p.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	stale := claim("stale")
	var refusal *RefusedError
	if err := s.Propose(ctx, stale.ID, proposal(7)); !errors.As(err, &refusal) || refusal.Reason != "stale_review_revision" {
		t.Fatalf("a stale revision: %v", err)
	}
	if cmd, _ := s.Command(ctx, stale.ID); cmd.State != "REJECTED" || cmd.Reason != "stale_review_revision" || proposals() != 0 {
		t.Fatalf("%+v with %d proposals, want REJECTED and none", cmd, proposals())
	}

	good := claim("good")
	if err := s.Propose(ctx, good.ID, proposal(1)); err != nil {
		t.Fatal(err)
	}
	cmd, err := s.Command(ctx, good.ID)
	if err != nil || cmd.State != "APPLIED" || len(cmd.EffectIDs) != 1 || proposals() != 1 {
		t.Fatalf("%+v (%v) with %d proposals, want APPLIED with one effect and one proposal", cmd, err, proposals())
	}
	var stored string
	if err := s.Pool().QueryRow(ctx, "select id::text from fx.resolution_proposal where payment_id = $1", p.ID).Scan(&stored); err != nil || stored != cmd.EffectIDs[0] {
		t.Fatalf("the command names %v, the proposal is %s (%v)", cmd.EffectIDs, stored, err)
	}

	if err := s.Propose(ctx, good.ID, proposal(1)); err != nil || proposals() != 1 {
		t.Fatalf("a retry of a settled command stored %d proposals (%v), want 1", proposals(), err)
	}

	racing := claim("racing")
	var wg sync.WaitGroup
	for range 6 {
		wg.Go(func() {
			if err := s.Propose(ctx, racing.ID, proposal(1)); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if proposals() != 2 {
		t.Fatalf("%d proposals after the racing retries, want the settled one and a single new one", proposals())
	}
}

func TestAProposalBeyond32BitsIsStoredWithItsEvidence(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := context.Background()
	const amount int64 = math.MaxInt32 + 1
	p := admitted(t, s, "k-1")
	payout := p.Payout.ID.String()
	if _, err := s.Pool().Exec(ctx, `
		update fx.payment set review_open = true, review_reason = 'PAYOUT_UNCONFIRMED', review_revision = 1, review_opened_at = now() where id = $1`, p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool().Exec(ctx, "update fx.payout set amount = $2 where payment_id = $1", p.ID, amount); err != nil {
		t.Fatal(err)
	}
	evidenceID, digest, err := RecordEvidence(ctx, s.Pool(), Evidence{
		Source: "READ", Operation: "PAYOUT", Reference: payout, Outcome: "COMPLETED", Currency: "USD", Amount: amount, Payload: map[string]int64{"amount": amount},
	})
	if err != nil {
		t.Fatalf("evidence of %d: %v", amount, err)
	}
	cmd, _, err := s.ClaimCommand(ctx, CommandClaim{ActorKind: "OPERATOR", ActorID: "alice", RequestKey: "paid", Operation: "PROPOSE_RESOLUTION", SubjectID: p.ID.String(), Request: "paid"}, false)
	if err != nil {
		t.Fatal(err)
	}
	err = s.Propose(ctx, cmd.ID, Proposal{
		PaymentID: p.ID, SubjectKind: "PAYOUT", SubjectID: payout, ReviewRevision: 1, Decision: "PAID", ProviderReference: payout,
		Currency: "USD", Amount: amount, EvidenceID: evidenceID, EvidenceDigest: digest, Maker: "alice",
	})
	if err != nil {
		t.Fatalf("a PAID proposal of %d: %v", amount, err)
	}
	var stored int64
	if err := s.Pool().QueryRow(ctx, "select amount from fx.resolution_proposal where payment_id = $1", p.ID).Scan(&stored); err != nil || stored != amount {
		t.Fatalf("stored %d (%v), want %d", stored, err, amount)
	}
}
