package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/xJohnNascimento/corridor/internal/pgtest"
)

func TestMain(m *testing.M) {
	os.Exit(pgtest.Main(m))
}

var beneficiary = Beneficiary{Name: "Northwind Traders", Account: "000123456789", BankCode: "NWBKUS33", Country: "US"}

func newStore(t *testing.T) *Store {
	t.Helper()
	s := New(pgtest.NewDatabase(t).App)
	for _, customer := range []string{"acme", "globex"} {
		if err := s.UpsertCustomer(context.Background(), customer, customer, KeyHash(customer)); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.UpsertOperator(context.Background(), "alice", "alice", KeyHash("alice")); err != nil {
		t.Fatal(err)
	}
	return s
}

func freezeQuote(t *testing.T, s *Store, customer, key string) Quote {
	t.Helper()
	return freezeQuoteFor(t, s, customer, key, 30*time.Second)
}

func freezeQuoteFor(t *testing.T, s *Store, customer, key string, ttl time.Duration) Quote {
	t.Helper()
	ctx := context.Background()
	cmd, _, err := s.ClaimCommand(ctx, CommandClaim{
		ActorKind: "CUSTOMER", ActorID: customer, RequestKey: key, Operation: "CREATE_QUOTE", Request: key,
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	q, err := s.FreezeQuote(ctx, pricedQuote(cmd.ID, customer, key), ttl)
	if err != nil {
		t.Fatal(err)
	}
	return q
}

func pricedQuote(command uuid.UUID, customer, key string) Quote {
	return Quote{
		CommandID: command, CustomerID: customer, SellCurrency: "CAD", BuyCurrency: "USD", SellAmount: 1000000, Fee: 5000, NetSell: 995000,
		ProviderRateE8: 73500000, CustomerRateE8: 73463250, ProviderBuy: 731325, Buy: 730959, Spread: 366, PolicyVersion: "test",
		ProviderQuoteID: "pq-" + key, ProviderExpiresAt: time.Now().Add(time.Minute),
	}
}

func admit(s *Store, customer, key string, quote uuid.UUID) (Payment, bool, error) {
	return s.Admit(context.Background(), AdmitParams{
		CustomerID: customer, RequestKey: key, RequestHash: Hash(key), QuoteID: quote, Beneficiary: beneficiary, Policy: map[string]string{},
	})
}

func admitted(t *testing.T, s *Store, key string) Payment {
	t.Helper()
	p, _, err := admit(s, "acme", key, freezeQuote(t, s, "acme", key).ID)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestTransitionInMovesARowOnceAndAnswersItsOwnReplay(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := context.Background()
	p := admitted(t, s, "k-1")

	steps := []struct {
		name    string
		table   Table
		t       Transition
		wantErr bool
	}{
		{"payment moves", PaymentTable, Transition{Key: "converting", From: []string{"ADMITTED"}, To: "CONVERTING", Event: "CONVERTING"}, false},
		{"the same key replays", PaymentTable, Transition{Key: "converting", From: []string{"ADMITTED"}, To: "CONVERTING", Event: "CONVERTING"}, false},
		{"a wrong from is refused", PaymentTable, Transition{Key: "converted", From: []string{"ADMITTED"}, To: "CONVERTED", Event: "CONVERTED"}, true},
		{"the target reached by another key is refused", PaymentTable, Transition{Key: "other", From: []string{"ADMITTED"}, To: "CONVERTING", Event: "CONVERTING"}, true},
		{"payment moves on", PaymentTable, Transition{Key: "converted", From: []string{"CONVERTING"}, To: "CONVERTED", Event: "CONVERTED"}, false},
		{"payout moves", PayoutTable, Transition{Key: "start-payout", From: []string{"PLANNED"}, To: "RESERVING", Event: "PAYOUT_RESERVING"}, false},
		{"the payout key replays", PayoutTable, Transition{Key: "start-payout", From: []string{"PLANNED"}, To: "RESERVING", Event: "PAYOUT_RESERVING"}, false},
		{"a payout wrong from is refused", PayoutTable, Transition{Key: "sent", From: []string{"SUBMITTING"}, To: "SENT", Event: "SENT"}, true},
	}
	for _, step := range steps {
		err := s.InTx(ctx, func(tx pgx.Tx) error { return TransitionIn(ctx, tx, step.table, p.ID, step.t) })
		switch {
		case step.wantErr && !errors.Is(err, ErrTransitionRefused):
			t.Errorf("%s: %v, want a refusal", step.name, err)
		case !step.wantErr && err != nil:
			t.Errorf("%s: %v", step.name, err)
		}
	}

	events, err := s.Timeline(ctx, p.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, e := range events {
		kinds = append(kinds, e.Kind)
	}
	want := []string{"ADMITTED", "CONVERTING", "CONVERTED", "PAYOUT_RESERVING"}
	if fmt.Sprint(kinds) != fmt.Sprint(want) {
		t.Fatalf("timeline %v, want %v: a replay must not repeat its event", kinds, want)
	}
}

func TestConcurrentFreezesOfOneQuoteAllReturnIt(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := context.Background()
	const rounds, callers = 100, 8
	for round := range rounds {
		key := fmt.Sprintf("q-%d", round)
		cmd, _, err := s.ClaimCommand(ctx, CommandClaim{ActorKind: "CUSTOMER", ActorID: "acme", RequestKey: key, Operation: "CREATE_QUOTE", Request: key}, false)
		if err != nil {
			t.Fatal(err)
		}
		quote := pricedQuote(cmd.ID, "acme", key)
		start := make(chan struct{})
		ids := make([]uuid.UUID, callers)
		errs := make([]error, callers)
		var wg sync.WaitGroup
		for i := range callers {
			wg.Go(func() {
				<-start
				frozen, err := s.FreezeQuote(ctx, quote, 30*time.Second)
				ids[i], errs[i] = frozen.ID, err
			})
		}
		close(start)
		wg.Wait()
		for i := range callers {
			if errs[i] != nil || ids[i] != cmd.ID {
				t.Fatalf("round %d, caller %d: quote %s, want %s: %v", round, i, ids[i], cmd.ID, errs[i])
			}
		}
	}
}

func TestAdmitSameKeyRacesToOnePayment(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	quote := freezeQuote(t, s, "acme", "q")
	const callers = 8
	var wg sync.WaitGroup
	payments := make([]Payment, callers)
	replays := make([]bool, callers)
	errs := make([]error, callers)
	for i := range callers {
		wg.Go(func() { payments[i], replays[i], errs[i] = admit(s, "acme", "same-key", quote.ID) })
	}
	wg.Wait()
	var created int
	for i := range callers {
		if errs[i] != nil || payments[i].ID != payments[0].ID {
			t.Fatalf("caller %d: %v, payment %s, want %s", i, errs[i], payments[i].ID, payments[0].ID)
		}
		if !replays[i] {
			created++
		}
	}
	if created != 1 {
		t.Fatalf("%d callers created the payment, want 1", created)
	}
	if _, _, err := s.Admit(context.Background(), AdmitParams{
		CustomerID: "acme", RequestKey: "same-key", RequestHash: Hash("another"), QuoteID: quote.ID, Beneficiary: beneficiary, Policy: map[string]string{},
	}); !errors.Is(err, ErrIdempotencyMismatch) {
		t.Fatalf("the key with another request: %v", err)
	}
}

func TestAdmitRefusesAQuoteThatExpiredWhileItWaitedForTheLock(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := pgtest.NewDatabase(t)
	s := New(db.App)
	if err := s.UpsertCustomer(ctx, "acme", "acme", KeyHash("acme")); err != nil {
		t.Fatal(err)
	}
	quote := freezeQuoteFor(t, s, "acme", "q", 2*time.Second)

	hold, err := db.Owner.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = hold.Rollback(ctx) }()
	if _, err := hold.Exec(ctx, "select id from fx.quote where id = $1 for update", quote.ID); err != nil {
		t.Fatal(err)
	}
	admitted := make(chan error, 1)
	go func() {
		_, _, err := admit(s, "acme", "late", quote.ID)
		admitted <- err
	}()
	for waiting := 0; waiting == 0; {
		if err := db.Owner.QueryRow(ctx, "select count(*) from pg_stat_activity where datname = $1 and wait_event_type = 'Lock'", db.Name).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(time.Until(quote.ExpiresAt) + 300*time.Millisecond)
	if err := hold.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-admitted; !errors.Is(err, ErrQuoteExpired) {
		t.Fatalf("a quote that expired while Admit waited for its lock: %v, want ErrQuoteExpired", err)
	}
}

func TestAdmitSameQuoteRacesToOnePayment(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	quote := freezeQuote(t, s, "acme", "q")
	const callers = 8
	var wg sync.WaitGroup
	payments := make([]Payment, callers)
	errs := make([]error, callers)
	for i := range callers {
		wg.Go(func() { payments[i], _, errs[i] = admit(s, "acme", fmt.Sprintf("key-%d", i), quote.ID) })
	}
	wg.Wait()
	var winners []uuid.UUID
	for i := range callers {
		var used *QuoteUsedError
		switch {
		case errs[i] == nil:
			winners = append(winners, payments[i].ID)
		case !errors.As(errs[i], &used):
			t.Fatalf("caller %d: %v", i, errs[i])
		}
	}
	if len(winners) != 1 {
		t.Fatalf("%d payments admitted for one quote, want 1", len(winners))
	}
	var rows int
	if err := s.Pool().QueryRow(context.Background(), "select count(*) from fx.payment where quote_id = $1", quote.ID).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("%d payment rows (%v), want 1", rows, err)
	}
	if _, _, err := admit(s, "globex", "g", quote.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another customer's quote: %v", err)
	}
}

func TestTheCLIReadsAreScopedToTheirPaymentAndCustomer(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := context.Background()
	first, second := admitted(t, s, "k-first"), admitted(t, s, "k-second")
	other, _, err := admit(s, "globex", "k-other", freezeQuote(t, s, "globex", "k-other").ID)
	if err != nil {
		t.Fatal(err)
	}

	listed, err := s.PaymentSummaries(ctx, "acme")
	if err != nil || len(listed) != 2 || listed[0].ID != first.ID || listed[1].ID != second.ID {
		t.Fatalf("acme's payments: %+v, %v; want %s then %s", listed, err, first.ID, second.ID)
	}
	if listed[0].Status != "ADMITTED" || listed[0].PayoutStatus != "PLANNED" || !listed[0].StartPending {
		t.Fatalf("summary: %+v", listed[0])
	}

	subject, err := s.ReviewSubject(ctx, other.ID)
	if err != nil || subject.PayoutID != other.PayoutID || subject.Currency != "USD" || subject.Amount != 730959 || subject.Reason != "" {
		t.Fatalf("subject: %+v, %v", subject, err)
	}
	if _, err := s.ReviewSubject(ctx, uuid.New()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an unknown payment: %v, want ErrNotFound", err)
	}

	id, _, err := RecordEvidence(ctx, s.Pool(), Evidence{
		Source: "READ", Operation: "TRADE", Reference: first.ID.String(), Outcome: "FILLED", Currency: "USD", Amount: 730825, Payload: "x",
	})
	if err != nil {
		t.Fatal(err)
	}
	stored, err := uuid.Parse(id)
	if err != nil {
		t.Fatal(err)
	}
	if currency, amount, err := s.EvidenceAmount(ctx, stored); err != nil || currency != "USD" || amount != 730825 {
		t.Fatalf("evidence: %s %d, %v", currency, amount, err)
	}
	if _, _, err := s.EvidenceAmount(ctx, uuid.New()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown evidence: %v, want ErrNotFound", err)
	}
}
