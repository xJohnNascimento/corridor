package main

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	ledgerv1 "github.com/xJohnNascimento/ledger/api/ledger/v1"

	corridorv1 "github.com/xJohnNascimento/corridor/api/corridor/v1"
)

var corridors = [][2]string{
	{"BRL", "CAD"}, {"BRL", "EUR"}, {"BRL", "USD"}, {"CAD", "BRL"}, {"CAD", "EUR"}, {"CAD", "USD"},
	{"EUR", "BRL"}, {"EUR", "CAD"}, {"EUR", "USD"}, {"USD", "BRL"}, {"USD", "CAD"}, {"USD", "EUR"},
}

// happyPath pays CAD to USD alone first and checks every account it moves against the worked
// example to the minor unit, then pays every other corridor in parallel.
func happyPath(ctx context.Context, s *stack) error {
	house, err := s.houseAccounts(ctx, "CAD", "USD")
	if err != nil {
		return err
	}
	before, err := s.balances(ctx, house)
	if err != nil {
		return err
	}
	if err := payOnce(ctx, s, "CAD", "USD", quoteSell); err != nil {
		return fmt.Errorf("CAD→USD: %w", err)
	}
	after, err := s.balances(ctx, house)
	if err != nil {
		return err
	}
	var errs []error
	// Provider cash CAD also holds the customer's deposit, which this payment funded.
	for name, want := range map[string]int64{"cash CAD": quoteSell - quoteNet, "fee CAD": quoteFee, "cash USD": providerBuy - customerBuy, "spread USD": quoteSpread, "diff USD": 0} {
		if got := after[name] - before[name]; got != want {
			errs = append(errs, fmt.Errorf("%s changed by %d, want %d", name, got, want))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return err
	}

	var (
		wg sync.WaitGroup
		mu sync.Mutex
	)
	for _, pair := range corridors {
		if pair == [2]string{"CAD", "USD"} {
			continue
		}
		wg.Go(func() {
			if err := payOnce(ctx, s, pair[0], pair[1], quoteSell); err != nil {
				mu.Lock()
				errs = append(errs, fmt.Errorf("%s→%s: %w", pair[0], pair[1], err))
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	return errors.Join(errs...)
}

func payOnce(ctx context.Context, s *stack, sell, buy string, amount int64) error {
	c, p, err := s.settle(ctx, plan{prefix: "s01-" + strings.ToLower(sell+buy), sell: sell, buy: buy, amount: amount, funds: amount}, 3*time.Minute)
	if err != nil {
		return err
	}
	if err := expect("status", status(p), "COMPLETED"); err != nil {
		return err
	}
	left, err := s.funds(ctx, c, sell)
	if err != nil {
		return err
	}
	clearing, err := s.ensureWorker(ctx, ledgerv1.OwnerKind_OWNER_KIND_PAYMENT, p.Id, "clearing", buy)
	if err != nil {
		return err
	}
	cleared, err := s.balance(ctx, clearing)
	if err != nil {
		return err
	}
	return errors.Join(expect("customer "+sell+" left", left, 0), expect("clearing "+buy, cleared, 0))
}

// sameKey sends one CreatePayment three times at once with one key, then the key with another body.
func sameKey(ctx context.Context, s *stack) error {
	c, err := s.newCustomer(ctx, "s06", map[string]int64{"CAD": quoteSell})
	if err != nil {
		return err
	}
	q, err := c.quote(ctx, "CAD", "USD", quoteSell)
	if err != nil {
		return err
	}
	key := randomHex(8)
	var wg sync.WaitGroup
	receipts := make([]*corridorv1.Receipt, 3)
	errs := make([]error, 3)
	for i := range 3 {
		wg.Go(func() {
			client := s.customerClient(c.id, c.key).withKey(key)
			resp, err := client.payments.CreatePayment(ctx, connect.NewRequest(&corridorv1.CreatePaymentRequest{QuoteId: q.Id, Beneficiary: beneficiary}))
			errs[i] = err
			if err == nil {
				receipts[i] = resp.Msg.Receipt
			}
		})
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		return err
	}
	for _, receipt := range receipts[1:] {
		if !proto.Equal(receipt, receipts[0]) {
			return fmt.Errorf("receipts differ: %v and %v", receipts[0], receipt)
		}
	}
	listed, err := s.fx(ctx, "payments", "list", "--customer", c.id)
	if err != nil {
		return err
	}
	var ids []string
	for line := range strings.Lines(listed) {
		if id, _, _ := strings.Cut(strings.TrimSpace(line), " "); id != "" {
			ids = append(ids, id)
		}
	}
	if len(ids) != 1 || ids[0] != receipts[0].PaymentId {
		return fmt.Errorf("payments %v for one key, want only the receipt's %s", ids, receipts[0].PaymentId)
	}
	changed := s.customerClient(c.id, c.key).withKey(key)
	_, err = changed.payments.CreatePayment(ctx, connect.NewRequest(&corridorv1.CreatePaymentRequest{QuoteId: q.Id, Beneficiary: beneficiary, RemittanceInfo: "changed"}))
	if err == nil {
		return errors.New("a changed body was accepted")
	}
	if reasonOf(err) != corridorv1.ErrorReason_ERROR_REASON_IDEMPOTENCY_MISMATCH {
		return fmt.Errorf("a changed body answered: %w", err)
	}
	if err := s.composeRun(ctx, "stop", "fx-worker"); err != nil {
		return err
	}
	_, readErr := c.payment(ctx, receipts[0].PaymentId)
	_, timelineErr := c.payments.GetTimeline(ctx, connect.NewRequest(&corridorv1.GetTimelineRequest{PaymentId: receipts[0].PaymentId}))
	if err := s.composeRun(ctx, "start", "fx-worker"); err != nil {
		return err
	}
	if err := errors.Join(readErr, timelineErr); err != nil {
		return fmt.Errorf("reads with the worker stopped: %w", err)
	}
	_, err = c.waitFor(ctx, receipts[0].PaymentId, 3*time.Minute, ended)
	return err
}

// apiDiesAfterAdmission loses the API's answer right after the admission commit; the client never
// retries, and the dispatcher must still start and finish the payment.
func apiDiesAfterAdmission(ctx context.Context, s *stack) error {
	c, err := s.newCustomer(ctx, "s07", map[string]int64{"EUR": quoteSell})
	if err != nil {
		return err
	}
	q, err := c.quote(ctx, "EUR", "USD", quoteSell)
	if err != nil {
		return err
	}
	if _, err := s.fx(ctx, "failpoints", "arm", "--name", "admission.afterCommit", "--subject", c.id); err != nil {
		return err
	}
	_, err = c.pay(ctx, q.Id)
	if err == nil {
		return errors.New("the admission answered, want unavailable")
	}
	if reasonOf(err) != corridorv1.ErrorReason_ERROR_REASON_UNAVAILABLE {
		return fmt.Errorf("the admission answered, want unavailable: %w", err)
	}
	var id string
	for deadline := time.Now().Add(10 * time.Second); id == "" && time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		listed, err := s.fx(ctx, "payments", "list", "--customer", c.id)
		if err != nil {
			return err
		}
		id, _, _ = strings.Cut(listed, " ")
	}
	p, err := c.waitFor(ctx, id, 3*time.Minute, ended)
	if err != nil {
		return err
	}
	return expect("the orphaned admission", status(p), "COMPLETED")
}

func checkerCatchesBadData(ctx context.Context, _ *stack) error {
	cmd := exec.CommandContext(ctx, "go", "test", "-count=1", "-run", "Corrupted|Truncated|Malformed|Broken|WorkedExample", "./...")
	cmd.Dir = "../verification"
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("checker-negative fixtures: %w: %s", err, out)
	}
	return nil
}

func incompatibleWorkflowChange(ctx context.Context, _ *stack) error {
	out, err := exec.CommandContext(ctx, "make", "-C", "..", "replay-break").CombinedOutput()
	if err != nil {
		return fmt.Errorf("make replay-break: %w: %s", err, out)
	}
	return nil
}
