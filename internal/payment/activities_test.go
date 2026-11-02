package payment_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.temporal.io/sdk/temporal"

	"github.com/xJohnNascimento/corridor/internal/ledger"
	"github.com/xJohnNascimento/corridor/internal/payment"
	"github.com/xJohnNascimento/corridor/internal/pgtest"
	"github.com/xJohnNascimento/corridor/internal/provider"
	"github.com/xJohnNascimento/corridor/internal/store"
)

func TestMain(m *testing.M) {
	os.Exit(pgtest.Main(m))
}

const returnEntry = "00000000-0000-0000-0000-000000000001"

// countingLedger answers only the unreserved return a cancel posts when no reservation exists,
// and counts how many returns it was asked to post. failFirst makes the first call fail, as a
// crash between the cancel's two transitions would.
type countingLedger struct {
	payment.Ledger
	mu        sync.Mutex
	returns   int
	failFirst bool
	calls     int
}

func (l *countingLedger) ReturnUnreservedPayout(context.Context, ledger.Payout) (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls++
	if l.failFirst && l.calls == 1 {
		return "", errors.New("the ledger is unreachable")
	}
	l.returns++
	return returnEntry, nil
}

func (l *countingLedger) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.returns
}

// answeringProvider answers every payout read and create with one fixed payout.
type answeringProvider struct {
	provider.Provider
	payout provider.Payout
}

func (p answeringProvider) CreatePayout(context.Context, provider.PayoutRequest) (provider.Payout, error) {
	return p.payout, nil
}

func (p answeringProvider) Payout(context.Context, string) (provider.Payout, error) {
	return p.payout, nil
}

type paymentFixture struct {
	t       *testing.T
	db      pgtest.Database
	store   *store.Store
	ledger  *countingLedger
	acts    *payment.Activities
	payment store.Payment
}

// newPaymentFixture admits acme's payment of the worked example, CAD 10,000.00 to USD 7,309.59,
// and moves it to CONVERTED, so its payout is PLANNED and can be cancelled.
func newPaymentFixture(t *testing.T) *paymentFixture {
	t.Helper()
	ctx := context.Background()
	db := pgtest.NewDatabase(t)
	s := store.New(db.App)
	if err := s.UpsertCustomer(ctx, "acme", "acme", store.KeyHash("acme")); err != nil {
		t.Fatal(err)
	}
	for _, currency := range []string{"CAD", "USD"} {
		if err := s.SetCustomerAccount(ctx, "acme", currency, uuid.NewString()); err != nil {
			t.Fatal(err)
		}
	}
	cmd, _, err := s.ClaimCommand(ctx, store.CommandClaim{
		ActorKind: "CUSTOMER", ActorID: "acme", RequestKey: "quote", Operation: "CREATE_QUOTE", Request: "quote",
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	quote, err := s.FreezeQuote(ctx, store.Quote{
		CommandID: cmd.ID, CustomerID: "acme", SellCurrency: "CAD", BuyCurrency: "USD", SellAmount: 1000000, Fee: 5000, NetSell: 995000,
		ProviderRateE8: 73500000, CustomerRateE8: 73463250, ProviderBuy: 731325, Buy: 730959, Spread: 366, PolicyVersion: "test",
		ProviderQuoteID: "pq-1", ProviderExpiresAt: time.Now().Add(time.Minute),
	}, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	p, _, err := s.Admit(ctx, store.AdmitParams{
		CustomerID: "acme", RequestKey: "payment", RequestHash: store.Hash("payment"), QuoteID: quote.ID,
		Beneficiary: store.Beneficiary{Name: "Northwind", Account: "000123", BankCode: "NWBKUS33", Country: "US"}, Policy: map[string]string{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Owner.Exec(ctx, "update fx.payment set status = 'CONVERTED' where id = $1", p.ID); err != nil {
		t.Fatal(err)
	}
	l := &countingLedger{}
	return &paymentFixture{t: t, db: db, store: s, ledger: l, payment: p, acts: &payment.Activities{Store: s, Ledger: l}}
}

// cancel stores a REQUEST_CANCEL command under its own key and decides it, as the workflow's
// Update handler does.
func (f *paymentFixture) cancel(key string) payment.Decision {
	f.t.Helper()
	ctx := context.Background()
	cmd, _, err := f.store.ClaimCommand(ctx, store.CommandClaim{
		ActorKind: "CUSTOMER", ActorID: "acme", RequestKey: key, Operation: "REQUEST_CANCEL", SubjectID: f.payment.ID.String(), Request: key,
	}, true)
	if err != nil {
		f.t.Fatal(err)
	}
	d, err := f.acts.DecideCommand(ctx, f.payment.ID.String(), cmd.ID.String())
	if err != nil {
		f.t.Fatal(err)
	}
	return d
}

func (f *paymentFixture) acceptedCancel(key string) string {
	f.t.Helper()
	d := f.cancel(key)
	if d.State != payment.Accepted {
		f.t.Fatalf("the cancel %s is %s (%s), want ACCEPTED", key, d.State, d.Reason)
	}
	return d.CommandID
}

func (f *paymentFixture) commandStates() map[string]string {
	f.t.Helper()
	rows, err := f.db.Owner.Query(context.Background(), "select id::text, state || ':' || coalesce(reason, '') from fx.product_command where operation = 'REQUEST_CANCEL'")
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	states := map[string]string{}
	for rows.Next() {
		var id, state string
		if err := rows.Scan(&id, &state); err != nil {
			f.t.Fatal(err)
		}
		states[id] = state
	}
	return states
}

func (f *paymentFixture) statuses() (payout, paymentStatus string) {
	f.t.Helper()
	err := f.db.Owner.QueryRow(context.Background(), "select o.status, p.status from fx.payout o join fx.payment p on p.id = o.payment_id where p.id = $1", f.payment.ID).Scan(&payout, &paymentStatus)
	if err != nil {
		f.t.Fatal(err)
	}
	return payout, paymentStatus
}

func TestARetriedCancellationSucceeds(t *testing.T) {
	t.Parallel()
	f := newPaymentFixture(t)
	command := f.acceptedCancel("cancel")
	id := f.payment.ID.String()

	for attempt := 1; attempt <= 2; attempt++ {
		if err := f.acts.CancelPayout(context.Background(), id, command); err != nil {
			t.Fatalf("attempt %d: %v", attempt, err)
		}
	}
	if payout, status := f.statuses(); payout != "CANCELLED" || status != "PAYOUT_CANCELLED" {
		t.Errorf("the payout is %s and the payment %s", payout, status)
	}
	if n := f.ledger.count(); n != 1 {
		t.Errorf("%d returns posted, want 1", n)
	}
	if states := f.commandStates(); states[command] != "APPLIED:" {
		t.Errorf("the command is %s, want APPLIED", states[command])
	}
}

func TestACancellationOfAnotherCommandIsNotAReplay(t *testing.T) {
	t.Parallel()
	f := newPaymentFixture(t)
	command := f.acceptedCancel("cancel")
	id := f.payment.ID.String()
	if err := f.acts.CancelPayout(context.Background(), id, command); err != nil {
		t.Fatal(err)
	}

	err := f.acts.CancelPayout(context.Background(), id, uuid.NewString())
	if err == nil {
		t.Fatal("a payout cancelled by one command was reported as cancelled by another")
	}
	if n := f.ledger.count(); n != 1 {
		t.Errorf("%d returns posted, want 1", n)
	}
}

func TestACancellationCrashedBetweenItsTransitionsContinues(t *testing.T) {
	t.Parallel()
	f := newPaymentFixture(t)
	f.ledger.failFirst = true
	command := f.acceptedCancel("cancel")
	id := f.payment.ID.String()

	if err := f.acts.CancelPayout(context.Background(), id, command); err == nil {
		t.Fatal("the first attempt must fail in the ledger")
	}
	if payout, _ := f.statuses(); payout != "CANCELLING" {
		t.Fatalf("the payout is %s after the crash, want CANCELLING", payout)
	}
	if err := f.acts.CancelPayout(context.Background(), id, command); err != nil {
		t.Fatalf("the retry: %v", err)
	}
	if payout, status := f.statuses(); payout != "CANCELLED" || status != "PAYOUT_CANCELLED" {
		t.Errorf("the payout is %s and the payment %s", payout, status)
	}
	if n := f.ledger.count(); n != 1 {
		t.Errorf("%d returns posted, want 1", n)
	}
}

func TestOnlyTheFirstCancelOfAPayoutIsAccepted(t *testing.T) {
	t.Parallel()
	f := newPaymentFixture(t)
	first := f.acceptedCancel("cancel-1")
	for _, key := range []string{"cancel-2", "cancel-3"} {
		if d := f.cancel(key); d.State != "REJECTED" || d.Reason != "cancel_already_accepted" || d.Cancel {
			t.Errorf("%s: %+v, want REJECTED with cancel_already_accepted", key, d)
		}
	}
	if d := f.cancel("cancel-2"); d.State != "REJECTED" {
		t.Errorf("the same key again: %+v, want its own stored decision", d)
	}

	var stored string
	if err := f.db.Owner.QueryRow(context.Background(), "select cancel_command_id::text from fx.payout where payment_id = $1", f.payment.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != first {
		t.Fatalf("the payout points at command %s, want the first, %s", stored, first)
	}
	if err := f.acts.CancelPayout(context.Background(), f.payment.ID.String(), stored); err != nil {
		t.Fatal(err)
	}
	for id, state := range f.commandStates() {
		want := "REJECTED:cancel_already_accepted"
		if id == first {
			want = "APPLIED:"
		}
		if state != want {
			t.Errorf("command %s is %s, want %s", id, state, want)
		}
	}
}

func TestConcurrentCancelsWithDifferentKeysConvergeOnOneReturn(t *testing.T) {
	t.Parallel()
	f := newPaymentFixture(t)
	const cancels = 8
	decisions := make([]payment.Decision, cancels)
	var wg sync.WaitGroup
	for i := range cancels {
		wg.Go(func() { decisions[i] = f.cancel(fmt.Sprintf("cancel-%d", i)) })
	}
	wg.Wait()

	var accepted string
	for _, d := range decisions {
		if d.State == payment.Accepted {
			if accepted != "" {
				t.Fatalf("commands %s and %s were both accepted", accepted, d.CommandID)
			}
			accepted = d.CommandID
		}
	}
	if accepted == "" {
		t.Fatal("no cancel was accepted")
	}
	if err := f.acts.CancelPayout(context.Background(), f.payment.ID.String(), accepted); err != nil {
		t.Fatal(err)
	}
	applied := 0
	for id, state := range f.commandStates() {
		switch state {
		case "APPLIED:":
			applied++
		case "REJECTED:cancel_already_accepted":
		default:
			t.Errorf("command %s is %s", id, state)
		}
	}
	if applied != 1 || f.ledger.count() != 1 {
		t.Errorf("%d commands applied and %d returns, want 1 and 1", applied, f.ledger.count())
	}
}

func TestAPayoutOutcomeMatchesOnlyWithTheTermsItMustCarry(t *testing.T) {
	t.Parallel()
	f := newPaymentFixture(t)
	ctx := context.Background()
	id := f.payment.ID.String()
	reference := f.payment.Payout.ID.String()
	amount := fmt.Sprint(f.payment.Payout.Amount)
	hint := payment.Hint{EventID: "evt-1", Reference: reference}

	reads := map[string]func(*payment.Activities, string) (payment.PayoutOutcome, error){
		"SubmitPayout": func(a *payment.Activities, _ string) (payment.PayoutOutcome, error) { return a.SubmitPayout(ctx, id) },
		"PollPayout":   func(a *payment.Activities, _ string) (payment.PayoutOutcome, error) { return a.PollPayout(ctx, id) },
		"VerifyPayout": func(a *payment.Activities, status string) (payment.PayoutOutcome, error) {
			h := hint
			h.Status = status
			return a.VerifyPayout(ctx, id, h)
		},
	}
	full := provider.Payout{Reference: reference, Currency: "USD", Amount: amount}
	cases := []struct {
		name   string
		status string
		change func(*provider.Payout)
		want   bool
	}{
		{"a completed payout with its terms", "COMPLETED", func(*provider.Payout) {}, true},
		{"a completed payout without a reference", "COMPLETED", func(p *provider.Payout) { p.Reference = "" }, false},
		{"a completed payout without a currency", "COMPLETED", func(p *provider.Payout) { p.Currency = "" }, false},
		{"a completed payout without an amount", "COMPLETED", func(p *provider.Payout) { p.Amount = "" }, false},
		{"a completed payout of another amount", "COMPLETED", func(p *provider.Payout) { p.Amount = "1" }, false},
		{"a completed payout in another currency", "COMPLETED", func(p *provider.Payout) { p.Currency = "CAD" }, false},
		{"a fence on an unknown reference, without terms", "CANCELLED", func(p *provider.Payout) { p.Currency, p.Amount = "", "" }, true},
		{"a failed payout without terms", "FAILED", func(p *provider.Payout) { p.Currency, p.Amount = "", "" }, true},
		{"a rejected payout without terms", "REJECTED", func(p *provider.Payout) { p.Currency, p.Amount = "", "" }, true},
		{"a cancelled payout of another amount", "CANCELLED", func(p *provider.Payout) { p.Amount = "1" }, false},
		{"a failed payout in another currency", "FAILED", func(p *provider.Payout) { p.Currency = "CAD" }, false},
		{"a cancelled payout without a reference", "CANCELLED", func(p *provider.Payout) { p.Reference = "" }, false},
		{"a payout in progress without terms", "PROCESSING", func(p *provider.Payout) { p.Currency, p.Amount = "", "" }, true},
		{"a payout in progress of another amount", "PROCESSING", func(p *provider.Payout) { p.Amount = "1" }, false},
	}
	for _, c := range cases {
		for name, read := range reads {
			if c.status == "PROCESSING" && name == "SubmitPayout" {
				continue // it would also move the payout to SENT, which this test does not set up
			}
			t.Run(c.name+" through "+name, func(t *testing.T) {
				payout := full
				payout.Status = c.status
				c.change(&payout)
				a := *f.acts
				a.Provider = answeringProvider{payout: payout}
				out, err := read(&a, c.status)
				var app *temporal.ApplicationError
				if err != nil && (!errors.As(err, &app) || app.Type() != payment.ErrorPayoutProcessing) {
					t.Fatal(err)
				}
				if out.Matches != c.want {
					t.Errorf("Matches is %v, want %v", out.Matches, c.want)
				}
			})
		}
	}
}
