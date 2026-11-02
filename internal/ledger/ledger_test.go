package ledger_test

import (
	"context"
	"testing"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	ledgerv1 "github.com/xJohnNascimento/ledger/api/ledger/v1"
	"github.com/xJohnNascimento/ledger/api/ledger/v1/ledgerv1connect"

	"github.com/xJohnNascimento/corridor/internal/ledger"
	"github.com/xJohnNascimento/corridor/internal/money"
)

type fakeService struct {
	ledgerv1connect.LedgerServiceClient
	committed []*ledgerv1.CommitReservationRequest
	released  []*ledgerv1.ReleaseReservationRequest
}

func (f *fakeService) CommitReservation(_ context.Context, req *connect.Request[ledgerv1.CommitReservationRequest]) (*connect.Response[ledgerv1.CommitReservationResponse], error) {
	f.committed = append(f.committed, req.Msg)
	return connect.NewResponse(&ledgerv1.CommitReservationResponse{Entry: &ledgerv1.JournalEntry{Id: "entry-1"}}), nil
}

func (f *fakeService) ReleaseReservation(_ context.Context, req *connect.Request[ledgerv1.ReleaseReservationRequest]) (*connect.Response[ledgerv1.ReleaseReservationResponse], error) {
	f.released = append(f.released, req.Msg)
	return connect.NewResponse(&ledgerv1.ReleaseReservationResponse{Entry: &ledgerv1.JournalEntry{Id: "entry-2"}}), nil
}

type fakeIntents struct{}

func (fakeIntents) RecordIntent(context.Context, string, string, string, uuid.UUID, any) error {
	return nil
}

func (fakeIntents) ObserveIntent(context.Context, string, string, any) error {
	return nil
}

func newLedger(svc ledgerv1connect.LedgerServiceClient) *ledger.Ledger {
	accounts := ledger.Accounts{
		LedgerID:      "corridor",
		ProviderCash:  map[string]string{"CAD": "cash-cad", "USD": "cash-usd"},
		FeeRevenue:    map[string]string{"CAD": "fee-cad", "USD": "fee-usd"},
		SpreadRevenue: map[string]string{"CAD": "spread-cad", "USD": "spread-usd"},
		FXDifference:  map[string]string{"CAD": "diff-cad", "USD": "diff-usd"},
	}
	return ledger.New(svc, fakeIntents{}, nil, accounts)
}

// balance sums debits minus credits per asset; a journal posts only if every asset nets to zero.
func balance(t *testing.T, j *ledgerv1.Journal) map[string]int64 {
	t.Helper()
	net := map[string]int64{}
	for _, line := range j.Lines {
		amount, err := money.ParseAmount(line.Amount)
		if err != nil {
			t.Fatal(err)
		}
		if line.Side == ledgerv1.Side_SIDE_DEBIT {
			net[line.AssetCode] += amount
			continue
		}
		net[line.AssetCode] -= amount
	}
	return net
}

func TestAConversionJournalBalancesPerCurrencyWhateverTheProviderDelivered(t *testing.T) {
	t.Parallel()
	conversion := ledger.Conversion{
		PaymentID: uuid.New(), CustomerFunds: "customer-cad", Clearing: "clearing-usd", SellCurrency: "CAD", BuyCurrency: "USD",
		Sell: 1000000, Fee: 5000, Net: 995000, ProviderBuy: 731325, Buy: 730959, Spread: 366,
	}
	cases := map[string]struct {
		delivered  int64
		wantLines  int
		difference string
	}{
		"delivered less than quoted": {delivered: 731325 - 500, wantLines: 8, difference: "diff-usd"},
		"delivered as quoted":        {delivered: 731325, wantLines: 6},
		"delivered more than quoted": {delivered: 731325 + 500, wantLines: 8, difference: "diff-usd"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			svc := &fakeService{}
			if _, err := newLedger(svc).CommitConversion(context.Background(), conversion, "reservation-1", c.delivered); err != nil {
				t.Fatal(err)
			}
			journal := svc.committed[0].Journal
			for asset, net := range balance(t, journal) {
				if net != 0 {
					t.Errorf("%s nets %d", asset, net)
				}
			}
			if len(journal.Lines) != c.wantLines {
				t.Errorf("%d lines, want %d: %v", len(journal.Lines), c.wantLines, journal.Lines)
			}
			var hasDifference bool
			for _, line := range journal.Lines {
				hasDifference = hasDifference || line.AccountId == c.difference
			}
			if hasDifference != (c.difference != "") {
				t.Errorf("difference line present: %v, want %v", hasDifference, c.difference != "")
			}
		})
	}
}

func TestPayoutJournalsBalance(t *testing.T) {
	t.Parallel()
	payout := ledger.Payout{PaymentID: uuid.New(), PayoutID: uuid.New(), Clearing: "clearing-usd", CustomerFunds: "customer-usd", Currency: "USD", Amount: 731325}
	svc := &fakeService{}
	l := newLedger(svc)
	if _, err := l.CommitPayout(context.Background(), payout, "reservation-2"); err != nil {
		t.Fatal(err)
	}
	if _, err := l.ReleasePayout(context.Background(), payout, "reservation-2"); err != nil {
		t.Fatal(err)
	}
	for name, journal := range map[string]*ledgerv1.Journal{"commit": svc.committed[0].Journal, "return": svc.released[0].ReturnJournal} {
		if net := balance(t, journal)["USD"]; net != 0 || len(journal.Lines) != 2 {
			t.Errorf("%s: USD nets %d over %d lines", name, net, len(journal.Lines))
		}
	}
}
