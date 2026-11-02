package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"

	"connectrpc.com/connect"

	ledgerv1 "github.com/xJohnNascimento/ledger/api/ledger/v1"
	"github.com/xJohnNascimento/ledger/api/ledger/v1/ledgerv1connect"

	"github.com/xJohnNascimento/corridor/api/corridor/v1/corridorv1connect"
)

const (
	ledgerID      = "corridor"
	corridorURL   = "http://127.0.0.1:8080"
	internalURL   = "http://127.0.0.1:8081"
	ledgerURL     = "http://127.0.0.1:9090"
	dittoURL      = "http://127.0.0.1:9002"
	providerOwner = "ditto"
	houseOwner    = "house"
)

var currencies = []string{"BRL", "CAD", "EUR", "USD"}

// stack is the running system as a client sees it: Corridor's API, the ledger's API with the
// seed, worker and audit clients, Ditto's admin API and the fx CLI inside the API container.
type stack struct {
	http    *http.Client
	compose []string
	seed    ledgerv1connect.LedgerServiceClient
	worker  ledgerv1connect.LedgerServiceClient
	audit   ledgerv1connect.LedgerServiceClient
}

func newStack() (*stack, error) {
	for _, name := range []string{"LEDGER_SECRET_SEED", "LEDGER_SECRET_CORRIDOR_WORKER", "LEDGER_SECRET_CORRIDOR_AUDIT", "DITTO_ADMIN_KEY", "DITTO_KEY_CORRIDOR"} {
		if os.Getenv(name) == "" {
			return nil, fmt.Errorf("%s is not set; run through make, which loads .env", name)
		}
	}
	client := &http.Client{Timeout: 30 * time.Second}
	return &stack{
		http:    client,
		compose: strings.Fields(envOr("COMPOSE", "docker compose -f ../compose.yaml -f ../compose.dev.yaml")),
		seed:    ledgerv1connect.NewLedgerServiceClient(client, ledgerURL, bearer(os.Getenv("LEDGER_SECRET_SEED"))),
		worker:  ledgerv1connect.NewLedgerServiceClient(client, ledgerURL, bearer(os.Getenv("LEDGER_SECRET_CORRIDOR_WORKER"))),
		audit:   ledgerv1connect.NewLedgerServiceClient(client, ledgerURL, bearer(os.Getenv("LEDGER_SECRET_CORRIDOR_AUDIT"))),
	}, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// bearer authenticates unary calls with a key; every call these clients make is unary.
func bearer(key string) connect.ClientOption {
	return connect.WithInterceptors(connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			req.Header().Set("Authorization", "Bearer "+key)
			return next(ctx, req)
		}
	}))
}

// customer is one API customer with its own key; every call carries a fresh Idempotency-Key
// unless a scenario sets one.
type customer struct {
	id             string
	key            string
	idempotencyKey string
	quotes         corridorv1connect.QuoteServiceClient
	payments       corridorv1connect.PaymentServiceClient
	commands       corridorv1connect.CommandServiceClient
}

func (s *stack) customerClient(id, key string) *customer {
	c := &customer{id: id, key: key}
	options := connect.WithInterceptors(connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			req.Header().Set("Authorization", "Bearer "+c.key)
			if c.idempotencyKey != "" {
				req.Header().Set("Idempotency-Key", c.idempotencyKey)
			}
			return next(ctx, req)
		}
	}))
	c.quotes = corridorv1connect.NewQuoteServiceClient(s.http, corridorURL, options)
	c.payments = corridorv1connect.NewPaymentServiceClient(s.http, corridorURL, options)
	c.commands = corridorv1connect.NewCommandServiceClient(s.http, corridorURL, options)
	return c
}

func (c *customer) withKey(key string) *customer {
	c.idempotencyKey = key
	return c
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// fx runs the fx CLI inside the API container.
func (s *stack) fx(ctx context.Context, args ...string) (string, error) {
	return s.fxWithEnv(ctx, nil, args...)
}

// fxWithEnv also hands the CLI environment variables, as NAME=value pairs. The container's exec
// takes each value from this process's environment, so a secret never shows in an argument list.
func (s *stack) fxWithEnv(ctx context.Context, env []string, args ...string) (string, error) {
	argv := slices.Concat(s.compose[1:], []string{"exec", "-T"})
	for _, pair := range env {
		name, _, _ := strings.Cut(pair, "=")
		argv = append(argv, "-e", name)
	}
	argv = slices.Concat(argv, []string{"fx-api", "/fx", "cli"}, args)
	cmd := exec.CommandContext(ctx, s.compose[0], argv...) //nolint:gosec // the compose command is the operator's own COMPOSE setting
	cmd.Env = append(os.Environ(), env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("fx cli %v: %w: %s", args, err, stderr.String())
	}
	return strings.TrimSpace(stdout.String()), nil
}

func (s *stack) composeRun(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, s.compose[0], slices.Concat(s.compose[1:], args)...) //nolint:gosec // the compose command is the operator's own COMPOSE setting
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("compose %v: %w: %s", args, err, out)
	}
	return nil
}

func (s *stack) ditto(ctx context.Context, method, path, key string, body, into any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, dittoURL+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("ditto %s %s: %s %s", method, path, resp.Status, data)
	}
	if into == nil {
		return nil
	}
	return json.Unmarshal(data, into)
}

func (s *stack) ensure(ctx context.Context, kind ledgerv1.OwnerKind, owner, purpose, currency string) (string, error) {
	resp, err := s.seed.EnsureAccount(ctx, connect.NewRequest(&ledgerv1.EnsureAccountRequest{
		LedgerId: ledgerID, OwnerKind: kind, OwnerId: owner, Purpose: purpose, AssetCode: currency,
	}))
	if err != nil {
		return "", err
	}
	return resp.Msg.Account.Id, nil
}

// ensureWorker creates an account through the worker's client, the one Corridor itself uses, so
// the scenarios see exactly the accounts the payment flow posts to.
func (s *stack) ensureWorker(ctx context.Context, kind ledgerv1.OwnerKind, owner, purpose, currency string) (string, error) {
	resp, err := s.worker.EnsureAccount(ctx, connect.NewRequest(&ledgerv1.EnsureAccountRequest{
		LedgerId: ledgerID, OwnerKind: kind, OwnerId: owner, Purpose: purpose, AssetCode: currency,
	}))
	if err != nil {
		return "", err
	}
	return resp.Msg.Account.Id, nil
}

func (s *stack) balance(ctx context.Context, account string) (int64, error) {
	resp, err := s.audit.GetBalance(ctx, connect.NewRequest(&ledgerv1.GetBalanceRequest{AccountId: account}))
	if err != nil {
		return 0, err
	}
	var v int64
	_, err = fmt.Sscan(resp.Msg.Balance.Posted, &v)
	return v, err
}

// fund books a deposit or a liquidity top-up twice, with the same reference: one funding movement
// at Ditto and one ledger journal into provider cash.
func (s *stack) fund(ctx context.Context, reference, currency string, amount int64, credit string) error {
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
	_, err = s.seed.PostJournal(ctx, connect.NewRequest(&ledgerv1.PostJournalRequest{
		Command:  &ledgerv1.Reference{Namespace: "seed.funding", Reference: reference},
		LedgerId: ledgerID,
		Journal: &ledgerv1.Journal{Kind: "funding", ProductReference: reference, Lines: []*ledgerv1.JournalLine{
			{AccountId: cash, Side: ledgerv1.Side_SIDE_DEBIT, AssetCode: currency, Amount: fmt.Sprint(amount)},
			{AccountId: credit, Side: ledgerv1.Side_SIDE_CREDIT, AssetCode: currency, Amount: fmt.Sprint(amount)},
		}},
	}))
	return err
}

// providerBalances reads Corridor's balances at Ditto from a new statement.
func (s *stack) providerBalances(ctx context.Context) (map[string]int64, error) {
	var statement struct {
		Currencies []struct {
			Currency       string `json:"currency"`
			ClosingBalance string `json:"closing_balance"`
		} `json:"currencies"`
	}
	if err := s.ditto(ctx, http.MethodPost, "/v1/statements", os.Getenv("DITTO_KEY_CORRIDOR"), map[string]string{}, &statement); err != nil {
		return nil, err
	}
	balances := map[string]int64{}
	for _, c := range statement.Currencies {
		var v int64
		if _, err := fmt.Sscan(c.ClosingBalance, &v); err != nil {
			return nil, err
		}
		balances[c.Currency] = v
	}
	return balances, nil
}
