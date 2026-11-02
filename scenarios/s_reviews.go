package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	corridorv1 "github.com/xJohnNascimento/corridor/api/corridor/v1"
)

func inReview(reason corridorv1.ReviewReason) func(*corridorv1.Payment) bool {
	return func(p *corridorv1.Payment) bool {
		return p.InReview && p.ReviewReason == reason || ended(p) && !p.InReview
	}
}

func (s *stack) opsLine(ctx context.Context, args ...string) ([]string, error) {
	out, err := s.fx(ctx, append([]string{"ops"}, args...)...)
	if err != nil {
		return nil, err
	}
	return strings.Fields(out), nil
}

func (s *stack) evidence(ctx context.Context, kind, payment string) (string, string, error) {
	f, err := s.opsLine(ctx, "evidence", kind, "--payment", payment)
	if err != nil {
		return "", "", fmt.Errorf("evidence: %w", err)
	}
	if len(f) < 2 {
		return "", "", fmt.Errorf("evidence answered %v", f)
	}
	return f[0], f[1], nil
}

// propose returns the command state, its reason, and the proposal id and digest when stored.
func (s *stack) propose(ctx context.Context, as, payment, decision, evidence, digest string) (state, reason, proposal, proposalDigest string, err error) {
	args := []string{"propose", "--as", as, "--payment", payment, "--decision", decision}
	if evidence != "" {
		args = append(args, "--evidence", evidence, "--digest", digest)
	}
	f, err := s.opsLine(ctx, args...)
	switch {
	case err != nil:
		return "", "", "", "", err
	case len(f) == 3:
		return f[0], "", f[1], f[2], nil
	case len(f) == 2:
		return f[0], f[1], "", "", nil
	}
	return "", "", "", "", fmt.Errorf("propose answered %v", f)
}

func (s *stack) approve(ctx context.Context, as, proposal, digest string) (string, string, error) {
	f, err := s.opsLine(ctx, "approve", "--as", as, "--proposal", proposal, "--digest", digest)
	if err != nil {
		return "", "", fmt.Errorf("approve: %w", err)
	}
	if len(f) < 2 {
		return "", "", fmt.Errorf("approve answered %v", f)
	}
	if len(f) == 2 {
		return f[0], "", nil
	}
	return f[0], f[1], nil
}

// fillReview pays CAD to USD with a fill 1,000 units short of the quote, beyond the 10 bps
// tolerance, and returns once the payment waits in review.
func (s *stack) fillReview(ctx context.Context, prefix string) (*customer, string, error) {
	request := cadToUsd(prefix)
	request.beforePay = func(q *corridorv1.Quote) error {
		return s.armFault(ctx, "FX_FILL_DIFFERENCE", "quote", q.Id, map[string]any{"delta": fmt.Sprint(-beyondTolerance)})
	}
	c, paid, err := s.start(ctx, request)
	if err != nil {
		return nil, "", err
	}
	p, err := c.waitFor(ctx, paid.Receipt.PaymentId, time.Minute, inReview(corridorv1.ReviewReason_REVIEW_REASON_FILL_OUT_OF_TOLERANCE))
	if err != nil {
		return nil, "", err
	}
	if p.ReviewReason != corridorv1.ReviewReason_REVIEW_REASON_FILL_OUT_OF_TOLERANCE {
		return nil, "", fmt.Errorf("payment ended %s without a fill review", status(p))
	}
	return c, p.Id, nil
}

func fillBeyondTolerance(ctx context.Context, s *stack) error {
	house, err := s.houseAccounts(ctx, "CAD", "USD")
	if err != nil {
		return err
	}
	before, err := s.balances(ctx, house)
	if err != nil {
		return err
	}
	c, payment, err := s.fillReview(ctx, "s17")
	if err != nil {
		return err
	}
	evidence, digest, err := s.evidence(ctx, "read", payment)
	if err != nil {
		return err
	}
	if err := s.resolve(ctx, c, payment, "ACCEPT_FILL", evidence, digest, "COMPLETED"); err != nil {
		return err
	}
	after, err := s.balances(ctx, house)
	if err != nil {
		return err
	}
	return expect("difference booked", after["diff USD"]-before["diff USD"], beyondTolerance)
}

func statusLag(ctx context.Context, s *stack) error {
	c, paid, err := s.converted(ctx, "s18", payoutIs(corridorv1.PayoutStatus_PAYOUT_STATUS_WAITING_RELEASE))
	if err != nil {
		return err
	}
	if err := s.armFault(ctx, "STATUS_API_LAG", "payout", paid.Receipt.PayoutId, map[string]any{"duration_ms": 600000}); err != nil {
		return err
	}
	payment := paid.Receipt.PaymentId
	if _, err := c.waitFor(ctx, payment, 5*time.Minute, inReview(corridorv1.ReviewReason_REVIEW_REASON_PAYOUT_UNCONFIRMED)); err != nil {
		return err
	}
	evidence, digest, err := s.evidence(ctx, "statement", payment)
	if err != nil {
		return err
	}
	return s.resolve(ctx, c, payment, "PAID", evidence, digest, "COMPLETED")
}

func stuckPayout(ctx context.Context, s *stack) error {
	c, paid, err := s.converted(ctx, "s19", payoutIs(corridorv1.PayoutStatus_PAYOUT_STATUS_WAITING_RELEASE))
	if err != nil {
		return err
	}
	if err := s.armFault(ctx, "PAYOUT_STUCK_CANCELLABLE", "payout", paid.Receipt.PayoutId, nil); err != nil {
		return err
	}
	payment := paid.Receipt.PaymentId
	if _, err := c.waitFor(ctx, payment, 5*time.Minute, inReview(corridorv1.ReviewReason_REVIEW_REASON_PAYOUT_UNCONFIRMED)); err != nil {
		return err
	}
	if err := s.resolve(ctx, c, payment, "NOT_PAID", "", "", "PAYOUT_FAILED"); err != nil {
		return err
	}
	usd, err := s.funds(ctx, c, "USD")
	if err != nil {
		return err
	}
	return expect("customer USD after one return", usd, customerBuy)
}

// decided turns an ops answer into an error: a failed call, a proposal that was not applied (want
// is its state) or an approval that was refused (want is empty, so only REJECTED fails).
func decided(what, state, want, reason string, err error) error {
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	if state == "REJECTED" || want != "" && state != want {
		return fmt.Errorf("%s %s %s", what, state, reason)
	}
	return nil
}

// resolve has alice propose and bob approve, then waits for the payment to end as want.
func (s *stack) resolve(ctx context.Context, c *customer, payment, decision, evidence, digest, want string) error {
	state, reason, proposal, proposalDigest, err := s.propose(ctx, "alice", payment, decision, evidence, digest)
	if err := decided("proposal", state, "APPLIED", reason, err); err != nil {
		return err
	}
	state, reason, err = s.approve(ctx, "bob", proposal, proposalDigest)
	if err := decided("approval", state, "", reason, err); err != nil {
		return err
	}
	p, err := c.waitFor(ctx, payment, 3*time.Minute, func(p *corridorv1.Payment) bool { return ended(p) && !p.InReview })
	if err != nil {
		return err
	}
	return expect("status", status(p), want)
}

// brokenApprovals tries every way an approval must be refused, checks the money did not move, and
// then resolves the review correctly.
func brokenApprovals(ctx context.Context, s *stack) error {
	c, payment, err := s.fillReview(ctx, "s20")
	if err != nil {
		return err
	}
	evidence, digest, err := s.evidence(ctx, "read", payment)
	if err != nil {
		return err
	}
	var errs []error
	refusedWith := func(what, state, reason, want string) {
		if state != "REJECTED" || reason != want {
			errs = append(errs, fmt.Errorf("%s: %s %s, want REJECTED %s", what, state, reason, want))
		}
	}
	state, reason, _, _, err := s.propose(ctx, "alice", payment, "PAID", evidence, digest)
	if err != nil {
		return err
	}
	refusedWith("a decision the review does not allow", state, reason, "decision_not_allowed_for_FILL_OUT_OF_TOLERANCE")
	state, reason, _, _, err = s.propose(ctx, "alice", payment, "ACCEPT_FILL", evidence, strings.Repeat("0", 64))
	if err != nil {
		return err
	}
	refusedWith("wrong evidence", state, reason, "evidence_digest_mismatch")

	_, _, first, firstDigest, err := s.propose(ctx, "alice", payment, "ACCEPT_FILL", evidence, digest)
	if err != nil {
		return err
	}
	state, reason, err = s.approve(ctx, "alice", first, firstDigest)
	if err != nil {
		return err
	}
	refusedWith("the maker approving", state, reason, "maker_cannot_approve")
	_, _, second, secondDigest, err := s.propose(ctx, "alice", payment, "ACCEPT_FILL", evidence, digest)
	if err != nil {
		return err
	}
	state, reason, err = s.approve(ctx, "bob", first, firstDigest)
	if err != nil {
		return err
	}
	refusedWith("a superseded proposal", state, reason, "proposal_superseded")
	reservation, err := s.reservationState(ctx, "corridor.conversion", payment)
	if err != nil {
		return err
	}
	if reservation != "RESERVATION_STATE_ACTIVE" {
		errs = append(errs, fmt.Errorf("after refused approvals the conversion reservation is %s", reservation))
	}
	if err := errors.Join(errs...); err != nil {
		return err
	}
	state, reason, err = s.approve(ctx, "bob", second, secondDigest)
	if err := decided("the valid approval", state, "", reason, err); err != nil {
		return err
	}
	p, err := c.waitFor(ctx, payment, 3*time.Minute, func(p *corridorv1.Payment) bool { return ended(p) && !p.InReview })
	if err != nil {
		return err
	}
	return expect("status", status(p), "COMPLETED")
}

// decisionSurvivesACrash makes the dispatcher fail right after it accepted the decision; the
// retry must still carry it out. The scenario also reads the failpoint's remaining uses, because
// a failpoint that never fired would leave a payment that completed without the crash.
func decisionSurvivesACrash(ctx context.Context, s *stack) error {
	c, payment, err := s.fillReview(ctx, "s21")
	if err != nil {
		return err
	}
	evidence, digest, err := s.evidence(ctx, "read", payment)
	if err != nil {
		return err
	}
	_, _, proposal, proposalDigest, err := s.propose(ctx, "alice", payment, "ACCEPT_FILL", evidence, digest)
	if err != nil {
		return err
	}
	const failpoint = "command.afterAccepted"
	if _, err := s.fx(ctx, "failpoints", "arm", "--name", failpoint, "--subject", payment); err != nil {
		return err
	}
	if _, _, err := s.approve(ctx, "bob", proposal, proposalDigest); err != nil {
		return err
	}
	p, err := c.waitFor(ctx, payment, 3*time.Minute, func(p *corridorv1.Payment) bool { return ended(p) && !p.InReview })
	if err != nil {
		return err
	}
	remaining, err := s.failpointRemaining(ctx, failpoint)
	if err != nil {
		return err
	}
	return errors.Join(
		expect("status", status(p), "COMPLETED"),
		expect("uses left of "+failpoint, remaining, "0"),
	)
}
