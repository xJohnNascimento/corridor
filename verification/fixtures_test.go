package verification_test

import (
	"fmt"
	"testing"

	ledgerv1 "github.com/xJohnNascimento/ledger/api/ledger/v1"

	"google.golang.org/protobuf/types/known/timestamppb"

	corridorv1 "github.com/xJohnNascimento/corridor/api/corridor/v1"
	"github.com/xJohnNascimento/corridor/verification"
)

// fixture builds a consistent dataset by hand: every balance is folded from the entries added, so
// only the corruption a test applies can break an invariant.
type fixture struct {
	d        *verification.Dataset
	sequence map[string]int64
	posted   map[string]int64
}

func newFixture() *fixture {
	f := &fixture{
		d: &verification.Dataset{
			Ledger:  verification.Ledger{Accounts: map[string]*ledgerv1.Account{}, Balances: map[string]*ledgerv1.Balance{}},
			Running: map[string]bool{},
		},
		sequence: map[string]int64{}, posted: map[string]int64{},
	}
	for _, a := range []struct {
		id, owner, purpose, asset string
		kind                      ledgerv1.OwnerKind
		side                      ledgerv1.Side
		enforced                  bool
	}{
		{"funds-cad", "acme", "funds", "CAD", ledgerv1.OwnerKind_OWNER_KIND_CUSTOMER, ledgerv1.Side_SIDE_CREDIT, true},
		{"funds-usd", "acme", "funds", "USD", ledgerv1.OwnerKind_OWNER_KIND_CUSTOMER, ledgerv1.Side_SIDE_CREDIT, true},
		{"cash-cad", "ditto", "cash", "CAD", ledgerv1.OwnerKind_OWNER_KIND_PROVIDER, ledgerv1.Side_SIDE_DEBIT, true},
		{"cash-usd", "ditto", "cash", "USD", ledgerv1.OwnerKind_OWNER_KIND_PROVIDER, ledgerv1.Side_SIDE_DEBIT, true},
		{"equity-usd", "house", "funding_equity", "USD", ledgerv1.OwnerKind_OWNER_KIND_HOUSE, ledgerv1.Side_SIDE_CREDIT, false},
		{"fee-cad", "house", "fee_revenue", "CAD", ledgerv1.OwnerKind_OWNER_KIND_HOUSE, ledgerv1.Side_SIDE_CREDIT, false},
		{"spread-usd", "house", "spread_revenue", "USD", ledgerv1.OwnerKind_OWNER_KIND_HOUSE, ledgerv1.Side_SIDE_CREDIT, false},
		{"diff-usd", "house", "fx_difference", "USD", ledgerv1.OwnerKind_OWNER_KIND_HOUSE, ledgerv1.Side_SIDE_DEBIT, false},
		{"clearing-usd", "p-1", "clearing", "USD", ledgerv1.OwnerKind_OWNER_KIND_PAYMENT, ledgerv1.Side_SIDE_CREDIT, true},
		{"funds-usd-globex", "globex", "funds", "USD", ledgerv1.OwnerKind_OWNER_KIND_CUSTOMER, ledgerv1.Side_SIDE_CREDIT, true},
	} {
		f.d.Ledger.Accounts[a.id] = &ledgerv1.Account{Id: a.id, LedgerId: "corridor", OwnerKind: a.kind, OwnerId: a.owner, Purpose: a.purpose,
			AssetCode: a.asset, NormalSide: a.side, EnforceAvailable: a.enforced}
	}
	return f
}

type leg struct {
	account string
	debit   bool
	amount  int64
}

func (f *fixture) entry(id, kind, product string, legs ...leg) {
	e := &ledgerv1.JournalEntry{Id: id, LedgerId: "corridor", Kind: kind, ProductReference: product}
	for i, l := range legs {
		account := f.d.Ledger.Accounts[l.account]
		side := ledgerv1.Side_SIDE_CREDIT
		if l.debit {
			side = ledgerv1.Side_SIDE_DEBIT
		}
		f.sequence[l.account]++
		e.Postings = append(e.Postings, &ledgerv1.Posting{EntryId: id, LineNo: int32(i + 1), AccountId: l.account, AssetCode: account.AssetCode,
			Side: side, Amount: fmt.Sprint(l.amount), AccountSequence: f.sequence[l.account]})
		if side == account.NormalSide {
			f.posted[l.account] += l.amount
		} else {
			f.posted[l.account] -= l.amount
		}
	}
	f.d.Ledger.Entries = append(f.d.Ledger.Entries, e)
}

func (f *fixture) move(currency, kind, reference string, amount int64) {
	var balance int64
	for _, m := range f.d.Provider.Movements {
		if m.Currency == currency {
			balance = m.BalanceAfter
		}
	}
	f.d.Provider.Movements = append(f.d.Provider.Movements, verification.Movement{Currency: currency,
		Sequence: int64(len(f.d.Provider.Movements) + 1), Amount: amount, BalanceAfter: balance + amount, Kind: kind, Reference: reference})
}

// seal folds the balances and closes the provider's currencies at their last balance.
func (f *fixture) seal() *verification.Dataset {
	for id := range f.d.Ledger.Accounts {
		f.d.Ledger.Balances[id] = &ledgerv1.Balance{AccountId: id, Posted: fmt.Sprint(f.posted[id]), Reserved: "0", OverdraftLimit: "0"}
	}
	closing := map[string]int64{}
	for _, m := range f.d.Provider.Movements {
		closing[m.Currency] += m.Amount
	}
	f.d.Provider.Currencies = nil
	for _, currency := range []string{"CAD", "USD"} {
		f.d.Provider.Currencies = append(f.d.Provider.Currencies, verification.ProviderCurrency{Currency: currency, Closing: closing[currency]})
	}
	return f.d
}

// The worked example, in minor units: CAD 10,000.00 sold, a 0.50% fee, a provider quote of
// USD 7,313.25 and a customer quote of USD 7,309.59 after a 0.05% spread.
const (
	sellAmount    = 1000000
	feeAmount     = 5000
	netAmount     = sellAmount - feeAmount
	providerBuy   = 731325
	customerBuy   = 730959
	spreadAmount  = providerBuy - customerBuy
	shortDelivery = 730825
)

// converted books the first half of the worked example: a CAD deposit, USD liquidity and the
// conversion with a delivered amount. The payout is still to come.
func converted(delivered int64) *fixture {
	f := newFixture()
	f.entry("deposit", "funding", "seed-acme-cad", leg{"cash-cad", true, sellAmount}, leg{"funds-cad", false, sellAmount})
	f.move("CAD", "FUNDING", "seed-acme-cad", sellAmount)
	f.entry("liquidity", "funding", "seed-usd", leg{"cash-usd", true, 100000}, leg{"equity-usd", false, 100000})
	f.move("USD", "FUNDING", "seed-usd", 100000)
	legs := []leg{{"funds-cad", true, sellAmount}, {"fee-cad", false, feeAmount}, {"cash-cad", false, netAmount},
		{"cash-usd", true, providerBuy}, {"clearing-usd", false, customerBuy}, {"spread-usd", false, spreadAmount}}
	if diff := providerBuy - delivered; diff > 0 {
		legs = append(legs, leg{"diff-usd", true, diff}, leg{"cash-usd", false, diff})
	}
	f.entry("conversion", "fx.conversion", "payment/p-1", legs...)
	f.move("CAD", "TRADE_SELL", "p-1", -netAmount)
	f.move("USD", "TRADE_BUY", "p-1", delivered)
	f.d.Provider.Outcomes = []verification.Outcome{{Operation: "TRADE", Reference: "p-1", Status: "FILLED", Currency: "USD", Amount: delivered}}
	f.d.Corridor.Payments = []*corridorv1.Payment{{
		Id: "p-1", Status: corridorv1.PaymentStatus_PAYMENT_STATUS_PAYING_OUT, Buy: &corridorv1.Money{Currency: "USD", Amount: fmt.Sprint(customerBuy)},
		Payout: &corridorv1.Payout{Id: "o-1", Status: corridorv1.PayoutStatus_PAYOUT_STATUS_SENT, Amount: &corridorv1.Money{Currency: "USD", Amount: fmt.Sprint(customerBuy)}},
	}}
	f.d.Running["p-1"] = true
	f.d.Corridor.Trades = []*corridorv1.TradeRecord{{PaymentId: "p-1", ProviderReference: "p-1", Status: "FILLED", EntryId: "conversion",
		Delivered: &corridorv1.Money{Currency: "USD", Amount: fmt.Sprint(delivered)}}}
	return f
}

// workedExample completes the conversion with a settled payout.
func workedExample(delivered int64) *fixture {
	f := converted(delivered)
	f.entry("settlement", "fx.payout", "payout/o-1", leg{"clearing-usd", true, customerBuy}, leg{"cash-usd", false, customerBuy})
	f.move("USD", "PAYOUT_DEBIT", "o-1", -customerBuy)
	f.d.Provider.Outcomes = append(f.d.Provider.Outcomes, verification.Outcome{Operation: "PAYOUT", Reference: "o-1", Status: "COMPLETED", Currency: "USD", Amount: customerBuy})
	f.d.Corridor.Payments[0].Status = corridorv1.PaymentStatus_PAYMENT_STATUS_COMPLETED
	f.d.Corridor.Payments[0].Payout.Status = corridorv1.PayoutStatus_PAYOUT_STATUS_COMPLETED
	delete(f.d.Running, "p-1")
	return f
}

// cancelledPayout ends the payout as cancelled before it reached the provider: the bought amount
// goes back to the customer with no reservation and the provider knows nothing of the payout.
func cancelledPayout() *fixture {
	f := converted(providerBuy)
	f.d.Provider.Outcomes = f.d.Provider.Outcomes[:1]
	f.entry("return", "fx.payout-return", "payout/o-1", leg{"clearing-usd", true, customerBuy}, leg{"funds-usd", false, customerBuy})
	f.d.Corridor.Payments[0].Status = corridorv1.PaymentStatus_PAYMENT_STATUS_PAYOUT_CANCELLED
	f.d.Corridor.Payments[0].Payout.Status = corridorv1.PayoutStatus_PAYOUT_STATUS_CANCELLED
	delete(f.d.Running, "p-1")
	return f
}

// failedPayout ends the payout as failed at the provider. With its return the money goes back to
// the customer and the payment is terminal; without it, the payment still waits in PAYING_OUT so
// that only the missing return is wrong.
func failedPayout(returned bool) *fixture {
	f := converted(providerBuy)
	f.d.Provider.Outcomes = append(f.d.Provider.Outcomes, verification.Outcome{Operation: "PAYOUT", Reference: "o-1", Status: "FAILED", Currency: "USD", Amount: customerBuy})
	f.d.Corridor.Payments[0].Payout.Status = corridorv1.PayoutStatus_PAYOUT_STATUS_FAILED
	f.d.Corridor.Payments[0].Payout.SubmissionStartedAt = timestamppb.Now()
	f.d.Ledger.Reservations = []*ledgerv1.Reservation{{
		Id: "r-1", State: ledgerv1.ReservationState_RESERVATION_STATE_RELEASED, FinalCommandId: "c-1",
		ReservationRef: &ledgerv1.Reference{Namespace: "corridor.payout", Reference: "o-1"},
	}}
	if returned {
		f.entry("return", "fx.payout-return", "payout/o-1", leg{"clearing-usd", true, customerBuy}, leg{"funds-usd", false, customerBuy})
		f.d.Corridor.Payments[0].Status = corridorv1.PaymentStatus_PAYMENT_STATUS_PAYOUT_FAILED
		delete(f.d.Running, "p-1")
	}
	return f
}

func TestTheWorkedExamplePasses(t *testing.T) {
	for _, delivered := range []int64{providerBuy, shortDelivery} {
		if r := verification.Verify(workedExample(delivered).seal()); r.Result != verification.Pass {
			t.Fatalf("delivered %d: %s %v", delivered, r.Result, r.Findings)
		}
	}
}

func TestAFailedPayoutWithItsReturnPasses(t *testing.T) {
	if r := verification.Verify(failedPayout(true).seal()); r.Result != verification.Pass {
		t.Fatalf("%s %v", r.Result, r.Findings)
	}
}

func TestACancelledPayoutNeverSubmittedPasses(t *testing.T) {
	if r := verification.Verify(cancelledPayout().seal()); r.Result != verification.Pass {
		t.Fatalf("%s %v", r.Result, r.Findings)
	}
}

func TestAFencedCancelledPayoutWithoutTermsPasses(t *testing.T) {
	f := failedPayout(true)
	f.d.Provider.Outcomes[1] = verification.Outcome{Operation: "PAYOUT", Reference: "o-1", Status: "CANCELLED"}
	if r := verification.Verify(f.seal()); r.Result != verification.Pass {
		t.Fatalf("%s %v", r.Result, r.Findings)
	}
}

func TestCorruptedSnapshotsFailTheirInvariant(t *testing.T) {
	cases := map[string]struct {
		build   func() *fixture
		corrupt func(*fixture)
		fails   string
		// also lists the invariants that cannot stay clean when this one breaks, with the reason.
		also map[string]string
	}{
		"a missing fill-difference leg": {build: func() *fixture { return workedExample(shortDelivery) }, corrupt: func(f *fixture) {
			conversion := f.d.Ledger.Entries[2]
			conversion.Postings = conversion.Postings[:6]
			f.sequence = map[string]int64{}
			f.posted = map[string]int64{}
			entries := f.d.Ledger.Entries
			f.d.Ledger.Entries = nil
			for _, e := range entries {
				var legs []leg
				for _, p := range e.Postings {
					legs = append(legs, leg{p.AccountId, p.Side == ledgerv1.Side_SIDE_DEBIT, mustInt(p.Amount)})
				}
				f.entry(e.Id, e.Kind, e.ProductReference, legs...)
			}
		}, fails: "V5", also: map[string]string{"V9": "the provider moved the delivered amount, the ledger a different one"}},
		"a second provider payout for one reference": {build: func() *fixture { return workedExample(shortDelivery) }, corrupt: func(f *fixture) {
			f.move("USD", "PAYOUT_DEBIT", "o-1", -customerBuy)
			f.d.Provider.Outcomes = append(f.d.Provider.Outcomes, verification.Outcome{Operation: "PAYOUT", Reference: "o-1", Status: "COMPLETED", Currency: "USD", Amount: customerBuy})
		}, fails: "V6", also: map[string]string{"V9": "the provider moved the amount twice, the ledger once"}},
		"a return next to a successful payout": {build: func() *fixture { return workedExample(shortDelivery) }, corrupt: func(f *fixture) {
			f.entry("return", "fx.payout-return", "payout/o-1", leg{"clearing-usd", true, customerBuy}, leg{"funds-usd", false, customerBuy})
		}, fails: "V6", also: map[string]string{
			"V4": "the return drains a clearing balance that was already zero",
			"V8": "the same negative clearing balance is not zero after completion",
		}},
		"a balance edited behind the ledger's back": {build: func() *fixture { return workedExample(shortDelivery) }, corrupt: func(f *fixture) {
			f.posted["fee-cad"] += 1
		}, fails: "V2"},
		"an entry that does not balance": {build: func() *fixture { return workedExample(shortDelivery) }, corrupt: func(f *fixture) {
			f.entry("unbalanced", "adjustment", "adjust-1", leg{"fee-cad", true, 100}, leg{"fee-cad", false, 90})
		}, fails: "V1"},
		"a reservation that does not add up to the balance": {build: func() *fixture { return workedExample(shortDelivery) }, corrupt: func(f *fixture) {
			f.d.Ledger.Reservations = []*ledgerv1.Reservation{{
				Id: "r-1", State: ledgerv1.ReservationState_RESERVATION_STATE_ACTIVE,
				Legs: []*ledgerv1.ReservationLeg{{AccountId: "funds-cad", MaxReduction: "500"}},
			}}
		}, fails: "V3"},
		"an enforced account below zero": {build: func() *fixture { return workedExample(shortDelivery) }, corrupt: func(f *fixture) {
			f.entry("overdraw", "adjustment", "adjust-2", leg{"funds-usd", true, 100}, leg{"equity-usd", false, 100})
		}, fails: "V4"},
		"a failed payout with no return": {build: func() *fixture { return failedPayout(false) }, corrupt: func(*fixture) {}, fails: "V7"},
		"a failed payout the provider never showed as unpaid": {build: func() *fixture { return failedPayout(true) }, corrupt: func(f *fixture) {
			f.d.Provider.Outcomes = f.d.Provider.Outcomes[:1]
		}, fails: "V7"},
		"a no-payment outcome for another reference": {build: func() *fixture { return failedPayout(true) }, corrupt: func(f *fixture) {
			f.d.Provider.Outcomes[1].Reference = "o-2"
		}, fails: "V7"},
		"a no-payment outcome for another amount": {build: func() *fixture { return failedPayout(true) }, corrupt: func(f *fixture) {
			f.d.Provider.Outcomes[1].Amount = customerBuy - 1
		}, fails: "V7"},
		"a submitted cancelled payout the provider never showed as unpaid": {build: cancelledPayout, corrupt: func(f *fixture) {
			f.d.Corridor.Payments[0].Payout.SubmissionStartedAt = timestamppb.Now()
		}, fails: "V7"},
		"a cancelled payout that was never submitted but the provider knows": {build: cancelledPayout, corrupt: func(f *fixture) {
			f.d.Provider.Outcomes = append(f.d.Provider.Outcomes, verification.Outcome{Operation: "PAYOUT", Reference: "o-1", Status: "CANCELLED", Currency: "USD", Amount: customerBuy})
		}, fails: "V7"},
		"a balanced return that credits another customer's funds": {build: func() *fixture { return failedPayout(true) }, corrupt: func(f *fixture) {
			entry := f.d.Ledger.Entries[len(f.d.Ledger.Entries)-1]
			entry.Postings[1].AccountId = "funds-usd-globex"
			f.posted["funds-usd"] -= customerBuy
			f.posted["funds-usd-globex"] += customerBuy
		}, fails: "V7"},
		"an accepted command on a finished payment": {build: func() *fixture { return workedExample(shortDelivery) }, corrupt: func(f *fixture) {
			f.d.Corridor.Commands = []*corridorv1.Command{{Id: "cancel-old", SubjectId: "p-1", State: corridorv1.CommandState_COMMAND_STATE_ACCEPTED}}
		}, fails: "V10"},
		"a clearing balance left after completion": {build: func() *fixture { return workedExample(shortDelivery) }, corrupt: func(f *fixture) {
			f.entry("leftover", "adjustment", "adjust-3", leg{"equity-usd", true, 100}, leg{"clearing-usd", false, 100})
		}, fails: "V8"},
		"a provider movement with no ledger entry": {build: func() *fixture { return workedExample(shortDelivery) }, corrupt: func(f *fixture) {
			f.move("USD", "FUNDING", "stray-funding", 500)
		}, fails: "V9"},
		"an unfinished payment with no workflow": {build: func() *fixture { return workedExample(shortDelivery) }, corrupt: func(f *fixture) {
			f.d.Corridor.Payments = append(f.d.Corridor.Payments, &corridorv1.Payment{
				Id: "p-2", Status: corridorv1.PaymentStatus_PAYMENT_STATUS_CONVERTING,
				Payout: &corridorv1.Payout{Id: "o-2", Status: corridorv1.PayoutStatus_PAYOUT_STATUS_PLANNED},
			})
		}, fails: "V10"},
		"maker approving their own proposal": {build: func() *fixture { return workedExample(shortDelivery) }, corrupt: func(f *fixture) {
			f.d.Corridor.Proposals = []*corridorv1.ProposalRecord{{Id: "r-1", PaymentId: "p-1", Maker: "alice", Approver: "alice", Digest: "x"}}
		}, fails: "V11"},
	}
	for name, c := range cases {
		t.Run(name+" fails "+c.fails, func(t *testing.T) {
			f := c.build()
			c.corrupt(f)
			r := verification.Verify(f.seal())
			if r.Result != verification.Fail || !r.Failed(c.fails) {
				t.Fatalf("%s with %v, want %s to fail", r.Result, r.Findings, c.fails)
			}
			for _, id := range verification.InvariantIDs() {
				if _, expected := c.also[id]; id != c.fails && !expected && r.Failed(id) {
					t.Errorf("%s failed as well: %v", id, r.Findings)
				}
			}
		})
	}
}

func TestATruncatedStatementIsIncompleteNeverPass(t *testing.T) {
	d := workedExample(providerBuy).seal()
	d.Incomplete = append(d.Incomplete, "provider statement: 9 of 12 lines")
	if r := verification.Verify(d); r.Result != verification.Incomplete {
		t.Fatalf("a truncated statement gave %s", r.Result)
	}
}

func TestAMalformedAmountIsIncompleteNeverPass(t *testing.T) {
	d := workedExample(providerBuy).seal()
	d.Ledger.Balances["fee-cad"].Posted = "5 000"
	r := verification.Verify(d)
	if r.Result != verification.Incomplete || len(r.Missing) != 1 {
		t.Fatalf("a malformed balance gave %s, missing %v", r.Result, r.Missing)
	}
}

func mustInt(s string) int64 {
	var v int64
	_, _ = fmt.Sscan(s, &v)
	return v
}
