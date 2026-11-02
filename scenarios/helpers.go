package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect"

	ledgerv1 "github.com/xJohnNascimento/ledger/api/ledger/v1"

	corridorv1 "github.com/xJohnNascimento/corridor/api/corridor/v1"
)

// newCustomer creates a customer funded exactly for its scenario, so scenarios never share a
// customer balance.
func (s *stack) newCustomer(ctx context.Context, prefix string, funds map[string]int64) (*customer, error) {
	id := prefix + "-" + randomHex(4)
	key := randomHex(32)
	if _, err := s.fxWithEnv(ctx, []string{"FX_CUSTOMER_KEY=" + key}, "customers", "upsert", "--id", id, "--name", id); err != nil {
		return nil, err
	}
	for currency, amount := range funds {
		account, err := s.ensure(ctx, ledgerv1.OwnerKind_OWNER_KIND_CUSTOMER, id, "funds", currency)
		if err != nil {
			return nil, err
		}
		if err := s.fund(ctx, "scenario-"+id+"-"+currency, currency, amount, account); err != nil {
			return nil, err
		}
	}
	return s.customerClient(id, key), nil
}

func reasonOf(err error) corridorv1.ErrorReason {
	var connectErr *connect.Error
	if !errors.As(err, &connectErr) {
		return corridorv1.ErrorReason_ERROR_REASON_UNSPECIFIED
	}
	for _, d := range connectErr.Details() {
		if v, valueErr := d.Value(); valueErr == nil {
			if detail, ok := v.(*corridorv1.ErrorDetail); ok {
				return detail.Reason
			}
		}
	}
	return corridorv1.ErrorReason_ERROR_REASON_UNSPECIFIED
}

// quote asks for a quote and waits out the provider's or the customer's budget when told to.
func (c *customer) quote(ctx context.Context, sell, buy string, amount int64) (*corridorv1.Quote, error) {
	key := randomHex(8)
	for {
		resp, err := c.withKey(key).quotes.CreateQuote(ctx, connect.NewRequest(&corridorv1.CreateQuoteRequest{
			SellCurrency: sell, BuyCurrency: buy, SellAmount: fmt.Sprint(amount),
		}))
		if err == nil {
			return resp.Msg.Quote, nil
		}
		if reasonOf(err) != corridorv1.ErrorReason_ERROR_REASON_RATE_LIMITED {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

var beneficiary = &corridorv1.Beneficiary{Name: "Northwind Traders", Account: "000123456789", BankCode: "NWBKUS33", Country: "US"}

func (c *customer) pay(ctx context.Context, quoteID string) (*corridorv1.CreatePaymentResponse, error) {
	resp, err := c.withKey(randomHex(8)).payments.CreatePayment(ctx, connect.NewRequest(&corridorv1.CreatePaymentRequest{
		QuoteId: quoteID, Beneficiary: beneficiary, RemittanceInfo: "invoice 42",
	}))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

func (c *customer) payment(ctx context.Context, id string) (*corridorv1.Payment, error) {
	resp, err := c.payments.GetPayment(ctx, connect.NewRequest(&corridorv1.GetPaymentRequest{PaymentId: id}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.Payment, nil
}

// waitFor polls the payment until done holds or the timeout passes. Reads come from Postgres, so
// they work with the workers stopped.
func (c *customer) waitFor(ctx context.Context, id string, timeout time.Duration, done func(*corridorv1.Payment) bool) (*corridorv1.Payment, error) {
	deadline := time.Now().Add(timeout)
	for {
		p, err := c.payment(ctx, id)
		if err == nil && done(p) {
			return p, nil
		}
		if time.Now().After(deadline) {
			if err != nil {
				return nil, err
			}
			return p, fmt.Errorf("payment %s stuck at %s (payout %s, review %v)", id, p.Status, p.Payout.GetStatus(), p.ReviewReason)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func ended(p *corridorv1.Payment) bool {
	switch p.Status {
	case corridorv1.PaymentStatus_PAYMENT_STATUS_COMPLETED, corridorv1.PaymentStatus_PAYMENT_STATUS_REJECTED,
		corridorv1.PaymentStatus_PAYMENT_STATUS_EXPIRED, corridorv1.PaymentStatus_PAYMENT_STATUS_PAYOUT_FAILED,
		corridorv1.PaymentStatus_PAYMENT_STATUS_PAYOUT_CANCELLED:
		return true
	default:
		return p.InReview
	}
}

func expect[T comparable](what string, got, want T) error {
	if got != want {
		return fmt.Errorf("%s: got %v, want %v", what, got, want)
	}
	return nil
}

func status(p *corridorv1.Payment) string {
	return strings.TrimPrefix(p.Status.String(), "PAYMENT_STATUS_")
}

// balances reads the posted balance of named accounts, so a scenario can assert exact changes on
// shared accounts such as provider cash.
func (s *stack) balances(ctx context.Context, accounts map[string]string) (map[string]int64, error) {
	out := map[string]int64{}
	for name, id := range accounts {
		v, err := s.balance(ctx, id)
		if err != nil {
			return nil, err
		}
		out[name] = v
	}
	return out, nil
}

func (s *stack) houseAccounts(ctx context.Context, sell, buy string) (map[string]string, error) {
	accounts := map[string]string{}
	for name, spec := range map[string]struct {
		kind     ledgerv1.OwnerKind
		owner    string
		purpose  string
		currency string
	}{
		"cash " + sell:  {ledgerv1.OwnerKind_OWNER_KIND_PROVIDER, providerOwner, "cash", sell},
		"cash " + buy:   {ledgerv1.OwnerKind_OWNER_KIND_PROVIDER, providerOwner, "cash", buy},
		"fee " + sell:   {ledgerv1.OwnerKind_OWNER_KIND_HOUSE, houseOwner, "fee_revenue", sell},
		"spread " + buy: {ledgerv1.OwnerKind_OWNER_KIND_HOUSE, houseOwner, "spread_revenue", buy},
		"diff " + buy:   {ledgerv1.OwnerKind_OWNER_KIND_HOUSE, houseOwner, "fx_difference", buy},
	} {
		id, err := s.ensureWorker(ctx, spec.kind, spec.owner, spec.purpose, spec.currency)
		if err != nil {
			return nil, err
		}
		accounts[name] = id
	}
	return accounts, nil
}
