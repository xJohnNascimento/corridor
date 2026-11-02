// Package providertest certifies a provider adapter against the guarantees Corridor relies on. It
// lives with the consumer, so a provider release that breaks a guarantee fails here when Corridor
// bumps its pin, and it runs unchanged against any adapter.
package providertest

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/xJohnNascimento/corridor/internal/provider"
)

// Sandbox funds the provider account under test; a sandbox that cannot is reported as skipped.
type Sandbox interface {
	Fund(ctx context.Context, reference, currency string, amount int64) error
}

func Run(t *testing.T, p provider.Provider, sandbox Sandbox) {
	ctx := context.Background()
	run := fmt.Sprintf("pt-%d", time.Now().UnixNano())
	if err := sandbox.Fund(ctx, run+"-cad", "CAD", 10_000_000); err != nil {
		t.Skipf("the sandbox cannot fund CAD: %v", err)
	}
	if err := sandbox.Fund(ctx, run+"-usd", "USD", 10_000_000); err != nil {
		t.Skipf("the sandbox cannot fund USD: %v", err)
	}

	t.Run("a quote is idempotent by client reference", func(t *testing.T) {
		first, err := p.CreateQuote(ctx, run+"-q1", "CAD", "USD", 100000)
		if err != nil {
			t.Fatal(err)
		}
		again, err := p.CreateQuote(ctx, run+"-q1", "CAD", "USD", 100000)
		if err != nil || again.ID != first.ID || again.BuyAmount != first.BuyAmount {
			t.Fatalf("replay: %+v, %v", again, err)
		}
		if _, err := p.CreateQuote(ctx, run+"-q1", "CAD", "USD", 100001); !errors.Is(err, provider.ErrConflict) {
			t.Fatalf("another body: %v", err)
		}
	})

	t.Run("a trade is idempotent and found by reference", func(t *testing.T) {
		q, err := p.CreateQuote(ctx, run+"-q2", "CAD", "USD", 100000)
		if err != nil {
			t.Fatal(err)
		}
		req := provider.TradeRequest{Reference: run + "-t2", QuoteID: q.ID, SellCurrency: "CAD", BuyCurrency: "USD", SellAmount: "100000"}
		first, err := p.CreateTrade(ctx, req)
		if err != nil || first.Status != "FILLED" {
			t.Fatalf("trade: %+v, %v", first, err)
		}
		again, err := p.CreateTrade(ctx, req)
		read, readErr := p.Trade(ctx, req.Reference)
		if err != nil || readErr != nil || again.TradeID != first.TradeID || read.TradeID != first.TradeID || read.BuyAmount != first.BuyAmount {
			t.Fatalf("replay %+v (%v), read %+v (%v)", again, err, read, readErr)
		}
		if _, err := p.Trade(ctx, run+"-never"); !errors.Is(err, provider.ErrNotVisible) {
			t.Fatalf("an unknown reference: %v", err)
		}
	})

	t.Run("a close fences an unknown trade reference for good", func(t *testing.T) {
		fence, err := p.CloseTrade(ctx, run+"-t3")
		if err != nil || fence.Status != "CLOSED" {
			t.Fatalf("close: %+v, %v", fence, err)
		}
		q, err := p.CreateQuote(ctx, run+"-q3", "CAD", "USD", 100000)
		if err != nil {
			t.Fatal(err)
		}
		late, err := p.CreateTrade(ctx, provider.TradeRequest{Reference: run + "-t3", QuoteID: q.ID, SellCurrency: "CAD", BuyCurrency: "USD", SellAmount: "100000"})
		if err != nil || late.Status != "CLOSED" {
			t.Fatalf("a create after the close: %+v, %v", late, err)
		}
	})

	payout := func(reference string, amount int64) provider.PayoutRequest {
		return provider.PayoutRequest{Reference: reference, Currency: "USD", Amount: strconv.FormatInt(amount, 10),
			Beneficiary: provider.Beneficiary{ID: "b-1", Name: "Northwind", Account: "000123456789", BankCode: "NWBKUS33", Country: "US"}}
	}

	t.Run("a cancel fences an unknown payout reference and moves no cash", func(t *testing.T) {
		fence, err := p.CancelPayout(ctx, run+"-p1")
		if err != nil || fence.Status != "CANCELLED" {
			t.Fatalf("cancel: %+v, %v", fence, err)
		}
		late, err := p.CreatePayout(ctx, payout(run+"-p1", 1000))
		if err != nil || late.Status != "CANCELLED" {
			t.Fatalf("a create after the cancel: %+v, %v", late, err)
		}
	})

	t.Run("an unexecuted payout can be cancelled, a processed one cannot", func(t *testing.T) {
		accepted, err := p.CreatePayout(ctx, payout(run+"-p2", 1000))
		if err != nil || accepted.Status != "ACCEPTED" {
			t.Fatalf("payout: %+v, %v", accepted, err)
		}
		if fence, err := p.CancelPayout(ctx, run+"-p2"); err != nil || fence.Status != "CANCELLED" {
			t.Fatalf("cancel of an accepted payout: %+v, %v", fence, err)
		}
		if _, err := p.CreatePayout(ctx, payout(run+"-p3", 1000)); err != nil {
			t.Fatal(err)
		}
		final := waitTerminal(t, p, run+"-p3")
		if fence, err := p.CancelPayout(ctx, run+"-p3"); err != nil || fence.Status != final.Status {
			t.Fatalf("cancel of a %s payout answered %+v, %v", final.Status, fence, err)
		}
		if again := waitTerminal(t, p, run+"-p3"); again.Status != final.Status || again.Sequence < final.Sequence {
			t.Fatalf("a terminal outcome changed from %+v to %+v", final, again)
		}
	})

	t.Run("statements roll forward exactly", func(t *testing.T) {
		first, err := p.CreateStatement(ctx, "")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := p.CreatePayout(ctx, payout(run+"-p4", 500)); err != nil {
			t.Fatal(err)
		}
		second, err := p.CreateStatement(ctx, first.ID)
		if err != nil {
			t.Fatal(err)
		}
		for i, c := range second.Currencies {
			if c.OpeningBalance != first.Currencies[i].ClosingBalance || c.FromSequence != first.Currencies[i].ToSequence+1 {
				t.Fatalf("%s opens at %s from %d after closing at %s through %d", c.Currency, c.OpeningBalance, c.FromSequence,
					first.Currencies[i].ClosingBalance, first.Currencies[i].ToSequence)
			}
		}
	})
}

func waitTerminal(t *testing.T, p provider.Provider, reference string) provider.Payout {
	t.Helper()
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(500 * time.Millisecond) {
		payout, err := p.Payout(context.Background(), reference)
		if err != nil {
			t.Fatal(err)
		}
		if payout.Status == provider.PayoutCompleted || payout.Status == provider.PayoutFailed || payout.Status == provider.PayoutCancelled {
			return payout
		}
	}
	t.Fatalf("payout %s never ended", reference)
	return provider.Payout{}
}
