package dispatch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"

	"github.com/xJohnNascimento/corridor/internal/pgtest"
	"github.com/xJohnNascimento/corridor/internal/store"
)

func TestMain(m *testing.M) {
	os.Exit(pgtest.Main(m))
}

type call struct {
	method   string
	workflow string
	bounded  bool
}

type fakeTemporal struct {
	client.Client
	mu          sync.Mutex
	calls       []call
	signalError error
	// onStart, when set, answers ExecuteWorkflow in place of the default success.
	onStart      func(ctx context.Context, workflowID string) error
	starting     int
	mostStarting int
}

func (f *fakeTemporal) record(ctx context.Context, method, workflow string) {
	deadline, ok := ctx.Deadline()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call{method: method, workflow: workflow, bounded: ok && time.Until(deadline) <= defaultLease/2})
}

func (f *fakeTemporal) ExecuteWorkflow(ctx context.Context, options client.StartWorkflowOptions, _ any, _ ...any) (client.WorkflowRun, error) {
	f.record(ctx, "start", options.ID)
	if f.onStart == nil {
		return nil, nil
	}
	f.mu.Lock()
	f.starting++
	f.mostStarting = max(f.mostStarting, f.starting)
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.starting--
		f.mu.Unlock()
	}()
	return nil, f.onStart(ctx, options.ID)
}

func (f *fakeTemporal) count(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c.method == method {
			n++
		}
	}
	return n
}

func (f *fakeTemporal) UpdateWorkflow(ctx context.Context, options client.UpdateWorkflowOptions) (client.WorkflowUpdateHandle, error) {
	f.record(ctx, "update", options.WorkflowID)
	return nil, errors.New("unreachable")
}

func (f *fakeTemporal) SignalWorkflow(ctx context.Context, workflowID, _, _ string, _ any) error {
	f.record(ctx, "signal", workflowID)
	return f.signalError
}

func newDispatcher(t *testing.T) (*Dispatcher, *fakeTemporal) {
	t.Helper()
	s := store.New(pgtest.NewDatabase(t).App)
	ctx := context.Background()
	if err := s.UpsertCustomer(ctx, "acme", "acme", store.KeyHash("acme")); err != nil {
		t.Fatal(err)
	}
	temporal := &fakeTemporal{}
	return &Dispatcher{Store: s, Temporal: temporal}, temporal
}

func admit(t *testing.T, d *Dispatcher, key string, started bool) store.Payment {
	t.Helper()
	ctx := context.Background()
	cmd, _, err := d.Store.ClaimCommand(ctx, store.CommandClaim{ActorKind: "CUSTOMER", ActorID: "acme", RequestKey: key, Operation: "CREATE_QUOTE", Request: key}, false)
	if err != nil {
		t.Fatal(err)
	}
	q, err := d.Store.FreezeQuote(ctx, store.Quote{
		CommandID: cmd.ID, CustomerID: "acme", SellCurrency: "CAD", BuyCurrency: "USD", SellAmount: 1000000, Fee: 5000, NetSell: 995000,
		ProviderRateE8: 73500000, CustomerRateE8: 73463250, ProviderBuy: 731325, Buy: 730959, Spread: 366, PolicyVersion: "test",
		ProviderQuoteID: "pq-" + key, ProviderExpiresAt: time.Now().Add(time.Minute),
	}, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	p, _, err := d.Store.Admit(ctx, store.AdmitParams{
		CustomerID: "acme", RequestKey: key, RequestHash: store.Hash(key), QuoteID: q.ID,
		Beneficiary: store.Beneficiary{Name: "Northwind", Account: "000123", BankCode: "NWBKUS33", Country: "US"}, Policy: map[string]string{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if started {
		if err := d.Store.ClearStartPending(ctx, p.ID); err != nil {
			t.Fatal(err)
		}
	}
	return p
}

func receive(t *testing.T, d *Dispatcher, event, reference, payload string) {
	t.Helper()
	_, err := d.Store.Pool().Exec(context.Background(), `
		insert into fx.webhook_inbox (provider, event_id, payload_hash, payload, reference) values ('ditto', $1, $2, $3, nullif($4, ''))`,
		event, store.KeyHash(event), payload, reference,
	)
	if err != nil {
		t.Fatal(err)
	}
}

func inboxStatus(t *testing.T, d *Dispatcher, event string) string {
	t.Helper()
	var status string
	if err := d.Store.Pool().QueryRow(context.Background(), "select status from fx.webhook_inbox where event_id = $1", event).Scan(&status); err != nil {
		t.Fatal(err)
	}
	return status
}

func TestAMalformedStoredHintDoesNotBlockTheBatch(t *testing.T) {
	t.Parallel()
	d, temporal := newDispatcher(t)
	p := admit(t, d, "k-1", true)
	payout := p.Payout.ID.String()

	receive(t, d, "no-status", payout, `{"event_id":"no-status","data":{"reference":"`+payout+`","sequence":2}}`)
	receive(t, d, "bad-sequence", payout, `{"event_id":"bad-sequence","data":{"reference":"`+payout+`","status":"PROCESSING","sequence":"two"}}`)
	receive(t, d, "no-data", payout, `{"event_id":"no-data"}`)
	receive(t, d, "not-a-payout", "not-a-uuid", `{"event_id":"not-a-payout","data":{"reference":"not-a-uuid","status":"COMPLETED","sequence":3}}`)
	receive(t, d, "unknown-payout", uuid.NewString(), `{"event_id":"unknown-payout","data":{"status":"COMPLETED","sequence":3}}`)
	receive(t, d, "good", payout, `{"event_id":"good","data":{"reference":"`+payout+`","status":"COMPLETED","sequence":3}}`)

	if _, err := d.hints(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"no-status": "SIGNALLED", "bad-sequence": "SIGNALLED", "no-data": "SIGNALLED", "good": "SIGNALLED",
		"not-a-payout": "NO_TARGET", "unknown-payout": "NO_TARGET",
	}
	for event, status := range want {
		if got := inboxStatus(t, d, event); got != status {
			t.Errorf("%s is %s, want %s", event, got, status)
		}
	}
	if len(temporal.calls) != 4 {
		t.Errorf("%d signals, want 4: %+v", len(temporal.calls), temporal.calls)
	}
}

func TestAHintWaitsForItsWorkflowToStartAndEndsWithoutOneThatExists(t *testing.T) {
	t.Parallel()
	d, temporal := newDispatcher(t)
	waiting := admit(t, d, "waiting", false)
	receive(t, d, "early", waiting.Payout.ID.String(), `{"event_id":"early","data":{"status":"COMPLETED","sequence":3}}`)
	if _, err := d.hints(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := inboxStatus(t, d, "early"); got != "PENDING" || len(temporal.calls) != 0 {
		t.Fatalf("a hint before the workflow started is %s with %d signals, want PENDING and none", got, len(temporal.calls))
	}

	started := admit(t, d, "started", true)
	receive(t, d, "gone", started.Payout.ID.String(), `{"event_id":"gone","data":{"status":"COMPLETED","sequence":3}}`)
	temporal.signalError = serviceerror.NewNotFound("no such workflow")
	if _, err := d.hints(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := inboxStatus(t, d, "gone"); got != "NO_TARGET" {
		t.Fatalf("a hint for a workflow that does not exist is %s", got)
	}
}

func TestEveryCallToTemporalIsBoundedByHalfTheLease(t *testing.T) {
	t.Parallel()
	d, temporal := newDispatcher(t)
	ctx := context.Background()
	started := admit(t, d, "started", true)
	admit(t, d, "pending", false)
	if _, _, err := d.Store.ClaimCommand(ctx, store.CommandClaim{
		ActorKind: "CUSTOMER", ActorID: "acme", RequestKey: "cancel", Operation: "REQUEST_CANCEL", SubjectID: started.ID.String(), Request: "cancel",
	}, true); err != nil {
		t.Fatal(err)
	}
	receive(t, d, "hint", started.Payout.ID.String(), `{"event_id":"hint","data":{"status":"COMPLETED","sequence":3}}`)

	for _, pass := range []func(context.Context) (int, error){d.starts, d.commands, d.hints} {
		if _, err := pass(ctx); err != nil {
			t.Fatal(err)
		}
	}
	methods := map[string]bool{}
	for _, c := range temporal.calls {
		methods[c.method] = true
		if !c.bounded {
			t.Errorf("%s of %s has no deadline within half the lease", c.method, c.workflow)
		}
	}
	if !methods["start"] || !methods["update"] || !methods["signal"] {
		t.Fatalf("the passes made %+v, want a start, an update and a signal", temporal.calls)
	}
}

// run starts the dispatcher's loops and stops them, and waits for them, when the test ends.
func run(t *testing.T, d *Dispatcher) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		d.Run(ctx, 20*time.Millisecond)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
}

func eventually(t *testing.T, within time.Duration, what string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatalf("after %s: %s", within, what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func hang(ctx context.Context, _ string) error {
	<-ctx.Done()
	return ctx.Err()
}

func TestStartsThatHangDoNotDelayCommandsOrHints(t *testing.T) {
	t.Parallel()
	d, temporal := newDispatcher(t)
	d.Lease = 20 * time.Second
	temporal.onStart = hang
	for i := range 20 {
		admit(t, d, fmt.Sprintf("pending-%d", i), false)
	}
	started := admit(t, d, "started", true)
	if _, _, err := d.Store.ClaimCommand(context.Background(), store.CommandClaim{
		ActorKind: "CUSTOMER", ActorID: "acme", RequestKey: "cancel", Operation: "REQUEST_CANCEL", SubjectID: started.ID.String(), Request: "cancel",
	}, true); err != nil {
		t.Fatal(err)
	}
	receive(t, d, "hint", started.Payout.ID.String(), `{"event_id":"hint","data":{"status":"COMPLETED","sequence":3}}`)

	run(t, d)
	eventually(t, 3*time.Second, "a command and a hint are still waiting behind starts that hang", func() bool {
		return temporal.count("update") > 0 && temporal.count("signal") > 0
	})
}

func TestALaneClaimsOnlyWhatItCanRunAtOnce(t *testing.T) {
	t.Parallel()
	d, temporal := newDispatcher(t)
	d.Lease = 20 * time.Second
	temporal.onStart = hang
	for i := range 3 * laneSize {
		admit(t, d, fmt.Sprintf("pending-%d", i), false)
	}

	run(t, d)
	eventually(t, 3*time.Second, "the starts did not reach the lane's concurrency", func() bool { return temporal.count("start") >= laneSize })
	time.Sleep(200 * time.Millisecond)
	var leased int
	if err := d.Store.Pool().QueryRow(context.Background(), "select count(*) from fx.payment where start_lease_until > now()").Scan(&leased); err != nil {
		t.Fatal(err)
	}
	if leased != laneSize {
		t.Errorf("%d starts leased while all are hanging, want %d", leased, laneSize)
	}
	temporal.mu.Lock()
	defer temporal.mu.Unlock()
	if temporal.mostStarting != laneSize {
		t.Errorf("%d starts at once, want %d", temporal.mostStarting, laneSize)
	}
}

func TestNoLeaseExpiresBeforeItsRowIsHandled(t *testing.T) {
	t.Parallel()
	d, temporal := newDispatcher(t)
	d.Lease = 2 * time.Second
	const rows = 20
	var (
		mu      sync.Mutex
		handled = map[string]bool{}
		expired []string
	)
	temporal.onStart = func(ctx context.Context, workflowID string) error {
		var valid bool
		id := strings.TrimPrefix(workflowID, "payment/")
		err := d.Store.Pool().QueryRow(context.Background(), "select start_lease_until > now() from fx.payment where id = $1", id).Scan(&valid)
		mu.Lock()
		handled[id] = true
		if err != nil || !valid {
			expired = append(expired, id)
		}
		mu.Unlock()
		return hang(ctx, id)
	}
	for i := range rows {
		admit(t, d, fmt.Sprintf("pending-%d", i), false)
	}

	run(t, d)
	eventually(t, 15*time.Second, "not every start was attempted", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(handled) == rows
	})
	mu.Lock()
	defer mu.Unlock()
	if len(expired) > 0 {
		t.Errorf("%d starts began after their lease had expired: %v", len(expired), expired)
	}
}
