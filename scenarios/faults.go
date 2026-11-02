package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"connectrpc.com/connect"

	ledgerv1 "github.com/xJohnNascimento/ledger/api/ledger/v1"

	corridorv1 "github.com/xJohnNascimento/corridor/api/corridor/v1"
)

func (s *stack) armFault(ctx context.Context, fault, kind, value string, args map[string]any) error {
	return s.armFaultTimes(ctx, fault, kind, value, args, 1)
}

func (s *stack) armFaultTimes(ctx context.Context, fault, kind, value string, args map[string]any, uses int) error {
	body := map[string]any{"fault": fault, "target": map[string]string{"kind": kind, "value": value}, "uses": uses}
	if args != nil {
		body["args"] = args
	}
	return s.ditto(ctx, http.MethodPost, "/admin/scenarios", os.Getenv("DITTO_ADMIN_KEY"), body, nil)
}

// faultFired counts how many times Ditto applied an armed fault to its target.
func (s *stack) faultFired(ctx context.Context, fault, kind, value string) (int, error) {
	var list struct {
		Arms []struct {
			Fault  string `json:"fault"`
			Target struct {
				Kind  string `json:"kind"`
				Value string `json:"value"`
			} `json:"target"`
			Consumptions []json.RawMessage `json:"consumptions"`
		} `json:"arms"`
	}
	if err := s.ditto(ctx, http.MethodGet, "/admin/scenarios", os.Getenv("DITTO_ADMIN_KEY"), nil, &list); err != nil {
		return 0, err
	}
	fired := 0
	for _, arm := range list.Arms {
		if arm.Fault == fault && arm.Target.Kind == kind && arm.Target.Value == value {
			fired += len(arm.Consumptions)
		}
	}
	return fired, nil
}

type dittoPayout struct {
	Status      string `json:"status"`
	FailureCode string `json:"failure_code"`
}

// dittoPayout reads the provider's own record of a payout; found is false when the reference was
// never submitted.
func (s *stack) dittoPayout(ctx context.Context, reference string) (dittoPayout, bool, error) {
	var p dittoPayout
	err := s.ditto(ctx, http.MethodGet, "/v1/payouts/by-reference/"+reference, os.Getenv("DITTO_KEY_CORRIDOR"), nil, &p)
	if err != nil && errorIs404(err) {
		return p, false, nil
	}
	return p, err == nil, err
}

func (s *stack) dittoTrade(ctx context.Context, reference string) (string, bool, error) {
	var t struct {
		Status    string `json:"status"`
		BuyAmount string `json:"buy_amount"`
	}
	err := s.ditto(ctx, http.MethodGet, "/v1/trades/by-reference/"+reference, os.Getenv("DITTO_KEY_CORRIDOR"), nil, &t)
	if err != nil && errorIs404(err) {
		return "", false, nil
	}
	return t.Status, err == nil, err
}

func errorIs404(err error) bool {
	return err != nil && strings.Contains(err.Error(), " 404 ")
}

func (s *stack) funds(ctx context.Context, c *customer, currency string) (int64, error) {
	account, err := s.ensure(ctx, ledgerv1.OwnerKind_OWNER_KIND_CUSTOMER, c.id, "funds", currency)
	if err != nil {
		return 0, err
	}
	return s.balance(ctx, account)
}

func (s *stack) available(ctx context.Context, account string) (int64, error) {
	resp, err := s.audit.GetBalance(ctx, connect.NewRequest(&ledgerv1.GetBalanceRequest{AccountId: account}))
	if err != nil {
		return 0, err
	}
	var v int64
	_, err = fmt.Sscan(resp.Msg.Balance.Available, &v)
	return v, err
}

// drain moves provider cash out to the house, like a treasury sweep, at Ditto and in the ledger,
// so that only leaving remains available. It returns what it moved, to be refunded later.
func (s *stack) drain(ctx context.Context, currency string, leaving int64, reference string) (int64, error) {
	cash, err := s.ensure(ctx, ledgerv1.OwnerKind_OWNER_KIND_PROVIDER, providerOwner, "cash", currency)
	if err != nil {
		return 0, err
	}
	available, err := s.available(ctx, cash)
	if err != nil {
		return 0, err
	}
	amount := available - leaving
	if amount <= 0 {
		return 0, nil
	}
	return amount, s.sweep(ctx, reference, currency, -amount)
}

func (s *stack) sweep(ctx context.Context, reference, currency string, amount int64) error {
	err := s.ditto(ctx, http.MethodPost, "/admin/funding", os.Getenv("DITTO_ADMIN_KEY"), map[string]string{
		"account": "corridor", "reference": reference, "currency": currency, "amount": fmt.Sprint(amount),
	}, nil)
	if err != nil {
		return err
	}
	cash, err := s.ensure(ctx, ledgerv1.OwnerKind_OWNER_KIND_PROVIDER, providerOwner, "cash", currency)
	if err != nil {
		return err
	}
	equity, err := s.ensure(ctx, ledgerv1.OwnerKind_OWNER_KIND_HOUSE, houseOwner, "funding_equity", currency)
	if err != nil {
		return err
	}
	debit, credit, value := cash, equity, amount
	if amount < 0 {
		debit, credit, value = equity, cash, -amount
	}
	_, err = s.seed.PostJournal(ctx, connect.NewRequest(&ledgerv1.PostJournalRequest{
		Command:  &ledgerv1.Reference{Namespace: "seed.treasury", Reference: reference},
		LedgerId: ledgerID,
		Journal: &ledgerv1.Journal{Kind: "treasury", ProductReference: reference, Lines: []*ledgerv1.JournalLine{
			{AccountId: debit, Side: ledgerv1.Side_SIDE_DEBIT, AssetCode: currency, Amount: fmt.Sprint(value)},
			{AccountId: credit, Side: ledgerv1.Side_SIDE_CREDIT, AssetCode: currency, Amount: fmt.Sprint(value)},
		}},
	}))
	return err
}

func payoutIs(statuses ...corridorv1.PayoutStatus) func(*corridorv1.Payment) bool {
	return func(p *corridorv1.Payment) bool {
		for _, status := range statuses {
			if p.Payout.GetStatus() == status {
				return true
			}
		}
		return ended(p)
	}
}

func (c *customer) cancel(ctx context.Context, paymentID string) (*corridorv1.Command, error) {
	resp, err := c.withKey(randomHex(8)).payments.RequestCancel(ctx, connect.NewRequest(&corridorv1.RequestCancelRequest{PaymentId: paymentID}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.Command, nil
}

func (c *customer) command(ctx context.Context, id string) (*corridorv1.Command, error) {
	resp, err := c.commands.GetCommand(ctx, connect.NewRequest(&corridorv1.GetCommandRequest{CommandId: id}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.Command, nil
}

func (s *stack) reservationState(ctx context.Context, namespace, reference string) (string, error) {
	resp, err := s.audit.GetReservation(ctx, connect.NewRequest(&ledgerv1.GetReservationRequest{
		Lookup: &ledgerv1.GetReservationRequest_ByReference{ByReference: &ledgerv1.ReservationLookup{
			LedgerId: ledgerID, ReservationRef: &ledgerv1.Reference{Namespace: namespace, Reference: reference},
		}},
	}))
	if err != nil {
		var connectErr *connect.Error
		if errors.As(err, &connectErr) && connectErr.Code() == connect.CodeNotFound {
			return "NONE", nil
		}
		return "", err
	}
	return resp.Msg.Reservation.State.String(), nil
}

// failpointRemaining reads how many uses of an armed failpoint are left; zero after it fired.
func (s *stack) failpointRemaining(ctx context.Context, name string) (string, error) {
	return s.fx(ctx, "failpoints", "remaining", "--name", name)
}

func (s *stack) waitHeld(ctx context.Context, name string) error {
	for deadline := time.Now().Add(2 * time.Minute); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		held, err := s.fx(ctx, "failpoints", "held", "--name", name)
		if err != nil {
			return err
		}
		if held == "true" {
			return nil
		}
	}
	return fmt.Errorf("nothing reached %s", name)
}

// plan describes one payment for a new customer funded exactly for it.
type plan struct {
	prefix    string
	sell      string
	buy       string
	amount    int64
	funds     int64
	beforePay func(*corridorv1.Quote) error
}

// cadToUsd is the worked example: CAD 10,000.00 to USD, funded to the cent.
func cadToUsd(prefix string) plan {
	return plan{prefix: prefix, sell: "CAD", buy: "USD", amount: quoteSell, funds: quoteSell}
}

// start creates the customer, quotes, runs beforePay, which arms a fault against the quote, and
// pays. It returns once the API has answered.
func (s *stack) start(ctx context.Context, p plan) (*customer, *corridorv1.CreatePaymentResponse, error) {
	c, err := s.newCustomer(ctx, p.prefix, map[string]int64{p.sell: p.funds})
	if err != nil {
		return nil, nil, err
	}
	q, err := c.quote(ctx, p.sell, p.buy, p.amount)
	if err != nil {
		return nil, nil, err
	}
	if p.beforePay != nil {
		if err := p.beforePay(q); err != nil {
			return nil, nil, err
		}
	}
	paid, err := c.pay(ctx, q.Id)
	if err != nil {
		return nil, nil, err
	}
	return c, paid, nil
}

// settle starts a payment and waits until it ends.
func (s *stack) settle(ctx context.Context, p plan, timeout time.Duration) (*customer, *corridorv1.Payment, error) {
	c, paid, err := s.start(ctx, p)
	if err != nil {
		return nil, nil, err
	}
	end, err := c.waitFor(ctx, paid.Receipt.PaymentId, timeout, ended)
	return c, end, err
}

// converted pays the worked example up to the given payout state.
func (s *stack) converted(ctx context.Context, prefix string, until func(*corridorv1.Payment) bool) (*customer, *corridorv1.CreatePaymentResponse, error) {
	c, paid, err := s.start(ctx, cadToUsd(prefix))
	if err != nil {
		return nil, nil, err
	}
	if until != nil {
		if _, err := c.waitFor(ctx, paid.Receipt.PaymentId, 2*time.Minute, until); err != nil {
			return nil, nil, err
		}
	}
	return c, paid, nil
}
