package main

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	corridorv1 "github.com/xJohnNascimento/corridor/api/corridor/v1"
)

const temporalUI = "http://localhost:8233/namespaces/default/workflows/"

// demo runs the acts that need the scenario code. The Makefile adds the proof and versioning
// acts, which are make verify and make replay-break.
func demo(ctx context.Context, s *stack) error {
	acts := []struct {
		title string
		run   func(context.Context, *stack) error
	}{
		{"A payment in about a second", fastPayments},
		{"Things break", thingsBreak},
		{"People in the loop", peopleInTheLoop},
	}
	for i, act := range acts {
		fmt.Printf("\nAct %d. %s\n", i+1, act.title)
		if err := act.run(ctx, s); err != nil {
			return fmt.Errorf("act %d: %w", i+1, err)
		}
	}
	return nil
}

func fastPayments(ctx context.Context, s *stack) error {
	const payments = 20
	c, err := s.newCustomer(ctx, "demo", map[string]int64{"CAD": payments * quoteSell})
	if err != nil {
		return err
	}
	var payCalls, toConverted []time.Duration
	firstAnswer := 0
	var first string
	for range payments {
		q, err := c.quote(ctx, "CAD", "USD", quoteSell)
		if err != nil {
			return err
		}
		started := time.Now()
		paid, err := c.pay(ctx, q.Id)
		if err != nil {
			return err
		}
		payCalls = append(payCalls, time.Since(started))
		id := paid.Receipt.PaymentId
		if pastConversion(paid.Payment) {
			firstAnswer++
		} else if _, err := c.waitFor(ctx, id, time.Minute, func(p *corridorv1.Payment) bool { return pastConversion(p) || ended(p) }); err != nil {
			return err
		}
		toConverted = append(toConverted, time.Since(started))
		if first == "" {
			first = id
		}
	}
	fmt.Printf("  %d payments of CAD 10,000.00 to USD; %d were converted in the first answer\n", payments, firstAnswer)
	fmt.Printf("  pay call       %s\n", percentiles(payCalls))
	fmt.Printf("  to CONVERTED   %s\n", percentiles(toConverted))

	p, err := c.waitFor(ctx, first, 3*time.Minute, ended)
	if err != nil {
		return err
	}
	fmt.Printf("  the payout completed behind the answer, after the cancel window: %s\n", status(p))
	fmt.Printf("  history: %s%s\n", temporalUI, "payment%2F"+first)
	return expect("status", status(p), "COMPLETED")
}

// pastConversion holds once the conversion is committed; the payout's reservation follows within
// milliseconds, so the first answer usually reads PAYING_OUT rather than CONVERTED.
func pastConversion(p *corridorv1.Payment) bool {
	switch p.GetStatus() {
	case corridorv1.PaymentStatus_PAYMENT_STATUS_CONVERTED, corridorv1.PaymentStatus_PAYMENT_STATUS_PAYING_OUT,
		corridorv1.PaymentStatus_PAYMENT_STATUS_COMPLETED, corridorv1.PaymentStatus_PAYMENT_STATUS_PAYOUT_FAILED,
		corridorv1.PaymentStatus_PAYMENT_STATUS_PAYOUT_CANCELLED:
		return true
	default:
		return false
	}
}

func percentiles(durations []time.Duration) string {
	slices.Sort(durations)
	at := func(q float64) time.Duration {
		return durations[int(q*float64(len(durations)-1))].Round(time.Millisecond)
	}
	return fmt.Sprintf("p50 %v  p95 %v  max %v", at(0.5), at(0.95), durations[len(durations)-1].Round(time.Millisecond))
}

// thingsBreak replays four failure scenarios; make verify in act 4 shows every payment ended once.
func thingsBreak(ctx context.Context, s *stack) error {
	for _, id := range []string{"S09", "S11", "S13", "S12"} {
		sc, ok := find(id)
		if !ok {
			return fmt.Errorf("the demo needs scenario %s, which is not in the catalogue", id)
		}
		started := time.Now()
		if err := sc.run(ctx, s); err != nil {
			return fmt.Errorf("%s: %w", id, err)
		}
		fmt.Printf("  %s  %-48s ended once  %5.1fs\n", id, sc.title, time.Since(started).Seconds())
	}
	return nil
}

// peopleInTheLoop pays out while the provider's status API lags. The first operator proposes
// NOT_PAID and the second approves it, but the provider's fence says the money left, so the
// decision is refused; the statement line then proves PAID, and that decision completes.
func peopleInTheLoop(ctx context.Context, s *stack) error {
	c, paid, err := s.converted(ctx, "demo-review", payoutIs(corridorv1.PayoutStatus_PAYOUT_STATUS_WAITING_RELEASE))
	if err != nil {
		return err
	}
	payment := paid.Receipt.PaymentId
	if err := s.armFault(ctx, "STATUS_API_LAG", "payout", paid.Receipt.PayoutId, map[string]any{"duration_ms": 600000}); err != nil {
		return err
	}
	if _, err := c.waitFor(ctx, payment, 5*time.Minute, inReview(corridorv1.ReviewReason_REVIEW_REASON_PAYOUT_UNCONFIRMED)); err != nil {
		return err
	}
	fmt.Printf("  payment %s is in review: PAYOUT_UNCONFIRMED\n", payment)

	_, _, proposal, digest, err := s.propose(ctx, "alice", payment, "NOT_PAID", "", "")
	if err != nil {
		return err
	}
	if _, _, err := s.approve(ctx, "bob", proposal, digest); err != nil {
		return err
	}
	if err := s.waitRevision(ctx, payment, 2); err != nil {
		return err
	}
	fmt.Println("  alice proposed NOT_PAID, bob approved, and the provider's fence refused it: the money had left")

	evidence, digest, err := s.evidence(ctx, "statement", payment)
	if err != nil {
		return err
	}
	if err := s.resolve(ctx, c, payment, "PAID", evidence, digest, "COMPLETED"); err != nil {
		return err
	}
	fmt.Println("  alice proposed PAID from the statement line, bob approved, and the payment completed")
	return nil
}

// waitRevision waits until the payment's open review reaches the revision, which happens when
// the workflow refuses a decision and reopens the review.
func (s *stack) waitRevision(ctx context.Context, payment string, revision int) error {
	want := fmt.Sprintf("revision=%d", revision)
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		out, err := s.fx(ctx, "ops", "reviews", "--as", "alice")
		if err != nil {
			return err
		}
		for line := range strings.Lines(out) {
			if strings.HasPrefix(line, payment+" ") && strings.Contains(line, want) {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return fmt.Errorf("the review of %s never reached %s", payment, want)
}
