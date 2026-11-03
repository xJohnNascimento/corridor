package api_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	corridorv1 "github.com/xJohnNascimento/corridor/api/corridor/v1"
	"github.com/xJohnNascimento/corridor/api/corridor/v1/corridorv1connect"
	"github.com/xJohnNascimento/corridor/internal/api"
	"github.com/xJohnNascimento/corridor/internal/config"
	"github.com/xJohnNascimento/corridor/internal/money"
	"github.com/xJohnNascimento/corridor/internal/pgtest"
	"github.com/xJohnNascimento/corridor/internal/provider"
	"github.com/xJohnNascimento/corridor/internal/store"
)

func TestMain(m *testing.M) {
	os.Exit(pgtest.Main(m))
}

type fakeProvider struct {
	provider.Provider
	mu         sync.Mutex
	rateE8     int64
	ttl        time.Duration
	quotes     map[string]provider.Quote
	quoteCalls int
	limited    bool
}

func (f *fakeProvider) CreateQuote(_ context.Context, reference, sell, buy string, amount int64) (provider.Quote, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.quoteCalls++
	if f.limited {
		return provider.Quote{}, &provider.RateLimitedError{RetryAfter: 3 * time.Second}
	}
	if q, ok := f.quotes[reference]; ok {
		return q, nil
	}
	bought, _ := money.MulDivFloor(amount, f.rateE8, money.RateScale)
	q := provider.Quote{
		ID: fmt.Sprintf("pq-%d", len(f.quotes)+1), ClientReference: reference, SellCurrency: sell, BuyCurrency: buy,
		SellAmount: money.Format(amount), RateE8: money.Format(f.rateE8), BuyAmount: money.Format(bought), ExpiresAt: time.Now().Add(f.ttl),
	}
	f.quotes[reference] = q
	return q, nil
}

type harness struct {
	t        *testing.T
	store    *store.Store
	provider *fakeProvider
	customer *httptest.Server
	internal *httptest.Server
}

const webhookSecret = "whsec_MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=" //nolint:gosec // a fixture key, not a credential

const configYAML = `
currencies: [BRL, CAD, EUR, USD]
corridors: [{sell: CAD, buy: USD}, {sell: USD, buy: CAD}]
limits: {min_sell: 10000, max_sell: 25000000}
pricing: {fee_bps: 50, spread_bps: 5, fill_tolerance_bps: 10}
quotes: {ttl: 30s, booking_margin: 15s, per_second: 1, burst: 5}
ledger: {id: corridor, provider_owner: ditto, house_owner: house}
profiles:
  demo: {cancel_grace: 20s, fast_path_wait: 2s, trade_safety: 5s, poll_initial: 2s, poll_max: 10s, poll_horizon: 2m, liquidity_retry: 10s}
`

func newHarness(t *testing.T) *harness {
	t.Helper()
	cfg, err := config.Parse([]byte(configYAML))
	if err != nil {
		t.Fatal(err)
	}
	policy, err := cfg.Policy("demo")
	if err != nil {
		t.Fatal(err)
	}
	db := pgtest.NewDatabase(t)
	h := &harness{t: t, store: store.New(db.App), provider: &fakeProvider{rateE8: 73500000, ttl: time.Minute, quotes: map[string]provider.Quote{}}}
	secrets, err := api.ParseWebhookSecrets(webhookSecret)
	if err != nil {
		t.Fatal(err)
	}
	server := api.New(api.Deps{Store: h.store, Config: cfg, Policy: policy, Provider: h.provider, AuditorKeyHash: store.KeyHash("auditor-key"), WebhookSecrets: secrets})
	h.customer = httptest.NewServer(server.CustomerHandler())
	h.internal = httptest.NewServer(server.InternalHandler())
	t.Cleanup(h.customer.Close)
	t.Cleanup(h.internal.Close)
	for _, c := range []string{"acme", "globex"} {
		if err := h.store.UpsertCustomer(context.Background(), c, c, store.KeyHash(c+"-key")); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.store.UpsertOperator(context.Background(), "alice", "alice", store.KeyHash("alice-key")); err != nil {
		t.Fatal(err)
	}
	return h
}

type credentials struct {
	key, idempotency string
}

func (c credentials) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		if c.key != "" {
			req.Header().Set("Authorization", "Bearer "+c.key)
		}
		if c.idempotency != "" {
			req.Header().Set("Idempotency-Key", c.idempotency)
		}
		return next(ctx, req)
	}
}

func (c credentials) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return func(ctx context.Context, spec connect.Spec) connect.StreamingClientConn {
		conn := next(ctx, spec)
		if c.key != "" {
			conn.RequestHeader().Set("Authorization", "Bearer "+c.key)
		}
		return conn
	}
}

func (c credentials) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next
}

func (h *harness) quotes(key, idempotency string) corridorv1connect.QuoteServiceClient {
	return corridorv1connect.NewQuoteServiceClient(h.customer.Client(), h.customer.URL, connect.WithInterceptors(credentials{key, idempotency}))
}

func (h *harness) quote(customer, idempotency, sell, buy, amount string) (*corridorv1.Quote, error) {
	resp, err := h.quotes(customer+"-key", idempotency).CreateQuote(context.Background(), connect.NewRequest(&corridorv1.CreateQuoteRequest{
		SellCurrency: sell, BuyCurrency: buy, SellAmount: amount,
	}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.Quote, nil
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

func TestBothPortsReportHealthWithoutAKey(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	for _, srv := range []*httptest.Server{h.customer, h.internal} {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/healthz", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("health: %d", resp.StatusCode)
		}
	}
}

func TestOperationsAndWebhooksAreNeverOnTheCustomerPort(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	for _, path := range []string{"/" + corridorv1connect.OpsServiceName + "/ListReviews", "/webhooks/ditto"} {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, h.customer.URL+path, strings.NewReader("{}"))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s on the customer port: %d", path, resp.StatusCode)
		}
	}
	ops := corridorv1connect.NewOpsServiceClient(h.internal.Client(), h.internal.URL, connect.WithInterceptors(credentials{key: "acme-key"}))
	_, err := ops.ListReviews(context.Background(), connect.NewRequest(&corridorv1.ListReviewsRequest{}))
	if reasonOf(err) != corridorv1.ErrorReason_ERROR_REASON_UNAUTHENTICATED {
		t.Fatalf("a customer key on the internal port: %v", err)
	}
}

func TestTheWorkedExamplePricesExactlyThroughTheAPI(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	q, err := h.quote("acme", "k-1", "CAD", "USD", "1000000")
	if err != nil {
		t.Fatal(err)
	}
	if q.Fee.Amount != "5000" || q.NetSell.Amount != "995000" || q.CustomerRateE8 != "73463250" || q.Buy.Amount != "730959" || q.Buy.Currency != "USD" {
		t.Fatalf("quote %+v", q)
	}
	if ttl := time.Until(q.ExpiresAt.AsTime()); ttl < 25*time.Second || ttl > 31*time.Second {
		t.Fatalf("the quote lives %s, want 30 s", ttl)
	}
	var spread int64
	err = h.store.Pool().QueryRow(context.Background(), "select spread from fx.quote where id = $1", q.Id).Scan(&spread)
	if err != nil || spread != 366 {
		t.Fatalf("spread %d (%v), want 366", spread, err)
	}
}

func TestConcurrentSameKeyQuotesCreateOneProviderQuote(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	const callers = 4
	var wg sync.WaitGroup
	ids := make([]string, callers)
	errs := make([]error, callers)
	for i := range callers {
		wg.Go(func() {
			q, err := h.quote("acme", "same-key", "CAD", "USD", "1000000")
			errs[i] = err
			if err == nil {
				ids[i] = q.Id
			}
		})
	}
	wg.Wait()
	for i := range callers {
		if errs[i] != nil || ids[i] != ids[0] {
			t.Fatalf("caller %d: %s, %v", i, ids[i], errs[i])
		}
	}
	if len(h.provider.quotes) != 1 {
		t.Fatalf("%d provider quotes, want 1", len(h.provider.quotes))
	}
	_, err := h.quote("acme", "same-key", "CAD", "USD", "1000001")
	if reasonOf(err) != corridorv1.ErrorReason_ERROR_REASON_IDEMPOTENCY_MISMATCH {
		t.Fatalf("a changed body: %v", err)
	}
	other, err := h.quote("globex", "same-key", "CAD", "USD", "1000000")
	if err != nil || other.Id == ids[0] {
		t.Fatalf("another customer's key space: %v, %v", other, err)
	}
}

func TestQuoteRequestsAreChecked(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	cases := map[string]struct {
		sell, buy, amount string
		want              corridorv1.ErrorReason
	}{
		"bad amount":       {"CAD", "USD", "10.5", corridorv1.ErrorReason_ERROR_REASON_VALIDATION_FAILED},
		"bad currency":     {"cad", "USD", "100000", corridorv1.ErrorReason_ERROR_REASON_VALIDATION_FAILED},
		"disabled pair":    {"BRL", "EUR", "100000", corridorv1.ErrorReason_ERROR_REASON_CORRIDOR_DISABLED},
		"below the limits": {"CAD", "USD", "9999", corridorv1.ErrorReason_ERROR_REASON_AMOUNT_OUT_OF_RANGE},
		"above the limits": {"CAD", "USD", "25000001", corridorv1.ErrorReason_ERROR_REASON_AMOUNT_OUT_OF_RANGE},
	}
	for name, c := range cases {
		if _, err := h.quote("acme", "k-"+name, c.sell, c.buy, c.amount); reasonOf(err) != c.want {
			t.Errorf("%s: %v, want %v", name, err, c.want)
		}
	}
	_, err := h.quotes("acme-key", "").CreateQuote(context.Background(), connect.NewRequest(&corridorv1.CreateQuoteRequest{SellCurrency: "CAD", BuyCurrency: "USD", SellAmount: "100000"}))
	if reasonOf(err) != corridorv1.ErrorReason_ERROR_REASON_VALIDATION_FAILED {
		t.Fatalf("a missing Idempotency-Key: %v", err)
	}
	_, err = h.quote("nobody", "k", "CAD", "USD", "100000")
	if reasonOf(err) != corridorv1.ErrorReason_ERROR_REASON_UNAUTHENTICATED {
		t.Fatalf("an unknown key: %v", err)
	}
}

func TestTheQuoteBudgetAndTheProvidersRetryAfter(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	for i := range 5 {
		if _, err := h.quote("acme", fmt.Sprintf("b-%d", i), "CAD", "USD", "100000"); err != nil {
			t.Fatalf("quote %d within the burst: %v", i, err)
		}
	}
	_, err := h.quote("acme", "b-over", "CAD", "USD", "100000")
	if reasonOf(err) != corridorv1.ErrorReason_ERROR_REASON_RATE_LIMITED {
		t.Fatalf("over the burst: %v", err)
	}
	h.provider.limited = true
	_, err = h.quote("globex", "p-1", "CAD", "USD", "100000")
	if reasonOf(err) != corridorv1.ErrorReason_ERROR_REASON_RATE_LIMITED {
		t.Fatalf("the provider's 429: %v", err)
	}
}

func TestAProviderQuoteTooShortToBookRefusesItsKeyForGood(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.provider.ttl = 40 * time.Second
	for range 2 {
		_, err := h.quote("acme", "short", "CAD", "USD", "1000000")
		if reasonOf(err) != corridorv1.ErrorReason_ERROR_REASON_QUOTE_EXPIRED {
			t.Fatalf("a provider quote that expires inside the booking margin: %v", err)
		}
	}
	if h.provider.quoteCalls != 1 {
		t.Fatalf("%d provider calls, want 1: the refused key is answered from its command", h.provider.quoteCalls)
	}
	h.provider.ttl = time.Minute
	if _, err := h.quote("acme", "fresh", "CAD", "USD", "1000000"); err != nil {
		t.Fatalf("a new key: %v", err)
	}
}

func TestAReplayOfAFrozenQuoteDoesNotSpendTheBudget(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	first, err := h.quote("acme", "b-0", "CAD", "USD", "100000")
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i < 5; i++ {
		if _, err := h.quote("acme", fmt.Sprintf("b-%d", i), "CAD", "USD", "100000"); err != nil {
			t.Fatalf("quote %d within the burst: %v", i, err)
		}
	}
	for range 3 {
		replay, err := h.quote("acme", "b-0", "CAD", "USD", "100000")
		if err != nil || replay.Id != first.Id {
			t.Fatalf("a replay with the budget spent: %v, %v", replay, err)
		}
	}
	_, err = h.quote("acme", "b-new", "CAD", "USD", "100000")
	if reasonOf(err) != corridorv1.ErrorReason_ERROR_REASON_RATE_LIMITED {
		t.Fatalf("a new key over the burst: %v", err)
	}
	var claimed int
	if err := h.store.Pool().QueryRow(context.Background(), "select count(*) from fx.product_command where request_key = 'b-new'").Scan(&claimed); err != nil {
		t.Fatal(err)
	}
	if claimed != 0 {
		t.Fatalf("a refused quote left %d commands behind, want none", claimed)
	}
	_, err = h.quote("acme", "b-new", "CAD", "USD", "100000")
	if reasonOf(err) != corridorv1.ErrorReason_ERROR_REASON_RATE_LIMITED {
		t.Fatalf("the retry of a refused key must pay the budget again: %v", err)
	}
}
