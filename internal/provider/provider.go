package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// Statuses the provider reports. Trade and payout statuses share a few words but are distinct
// vocabularies, so each carries its own prefix.
const (
	TradeFilled   = "FILLED"
	TradeRejected = "REJECTED"
	TradeClosed   = "CLOSED"

	PayoutCompleted = "COMPLETED"
	PayoutFailed    = "FAILED"
	PayoutCancelled = "CANCELLED"
	PayoutRejected  = "REJECTED"

	FenceClosed = "CLOSED"
)

const defaultRetryAfter = time.Second

var (
	ErrNotVisible = errors.New("the reference is not visible at the provider now")
	ErrConflict   = errors.New("the provider holds another request under this reference")
)

// RateLimitedError carries the provider's Retry-After, so a caller waits exactly that long.
type RateLimitedError struct {
	RetryAfter time.Duration
}

func (e *RateLimitedError) Error() string {
	return fmt.Sprintf("rate limited by the provider; retry after %s", e.RetryAfter)
}

type Quote struct {
	ID              string    `json:"quote_id"`
	ClientReference string    `json:"client_reference"`
	SellCurrency    string    `json:"sell_currency"`
	BuyCurrency     string    `json:"buy_currency"`
	SellAmount      string    `json:"sell_amount"`
	RateE8          string    `json:"rate_e8"`
	BuyAmount       string    `json:"buy_amount"`
	ExpiresAt       time.Time `json:"expires_at"`
}

type Trade struct {
	TradeID      string     `json:"trade_id,omitempty"`
	Reference    string     `json:"reference"`
	Status       string     `json:"status"`
	Reason       string     `json:"reason,omitempty"`
	SellCurrency string     `json:"sell_currency,omitempty"`
	BuyCurrency  string     `json:"buy_currency,omitempty"`
	SellAmount   string     `json:"sell_amount,omitempty"`
	BuyAmount    string     `json:"buy_amount,omitempty"`
	RateE8       string     `json:"rate_e8,omitempty"`
	ExecutedAt   *time.Time `json:"executed_at,omitempty"`
}

type Beneficiary struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Account  string `json:"account"`
	BankCode string `json:"bank_code"`
	Country  string `json:"country"`
}

type Payout struct {
	PayoutID    string `json:"payout_id,omitempty"`
	Reference   string `json:"reference"`
	Status      string `json:"status"`
	Sequence    int    `json:"sequence"`
	Currency    string `json:"currency,omitempty"`
	Amount      string `json:"amount,omitempty"`
	FailureCode string `json:"failure_code,omitempty"`
}

type PayoutRequest struct {
	Reference      string      `json:"reference"`
	Currency       string      `json:"currency"`
	Amount         string      `json:"amount"`
	Beneficiary    Beneficiary `json:"beneficiary"`
	RemittanceInfo string      `json:"remittance_info,omitempty"`
}

type TradeRequest struct {
	Reference    string `json:"reference"`
	QuoteID      string `json:"quote_id"`
	SellCurrency string `json:"sell_currency"`
	BuyCurrency  string `json:"buy_currency"`
	SellAmount   string `json:"sell_amount"`
}

type Fence struct {
	Reference string `json:"reference"`
	Status    string `json:"status"`
}

type StatementCurrency struct {
	Currency       string `json:"currency"`
	OpeningBalance string `json:"opening_balance"`
	ClosingBalance string `json:"closing_balance"`
	FromSequence   int64  `json:"from_sequence"`
	ToSequence     int64  `json:"to_sequence"`
}

type Statement struct {
	ID          string              `json:"statement_id"`
	CapturedAt  time.Time           `json:"captured_at"`
	ContentHash string              `json:"content_hash"`
	LineCount   int64               `json:"line_count"`
	Currencies  []StatementCurrency `json:"currencies"`
}

type Page struct {
	Lines      []json.RawMessage `json:"lines"`
	NextCursor string            `json:"next_cursor"`
}

// Provider is what Corridor relies on from an FX provider and payout rail. providertest certifies
// an implementation against the guarantees in Ditto's design.
type Provider interface {
	CreateQuote(ctx context.Context, clientReference, sell, buy string, sellAmount int64) (Quote, error)
	CreateTrade(ctx context.Context, req TradeRequest) (Trade, error)
	Trade(ctx context.Context, reference string) (Trade, error)
	CloseTrade(ctx context.Context, reference string) (Fence, error)
	CreatePayout(ctx context.Context, req PayoutRequest) (Payout, error)
	Payout(ctx context.Context, reference string) (Payout, error)
	CancelPayout(ctx context.Context, reference string) (Fence, error)
	CreateStatement(ctx context.Context, after string) (Statement, error)
	StatementPage(ctx context.Context, id, cursor string) (Page, error)
}

type Ditto struct {
	client  *http.Client
	baseURL string
	key     string
}

func NewDitto(client *http.Client, baseURL, key string) *Ditto {
	return &Ditto{client: client, baseURL: baseURL, key: key}
}

func (d *Ditto) CreateQuote(ctx context.Context, clientReference, sell, buy string, sellAmount int64) (Quote, error) {
	var q Quote
	err := d.do(ctx, http.MethodPost, "/v1/quotes", map[string]string{
		"client_reference": clientReference, "sell_currency": sell, "buy_currency": buy, "sell_amount": strconv.FormatInt(sellAmount, 10),
	}, &q)
	return q, err
}

func (d *Ditto) CreateTrade(ctx context.Context, req TradeRequest) (Trade, error) {
	var t Trade
	return t, d.do(ctx, http.MethodPost, "/v1/trades", req, &t)
}

func (d *Ditto) Trade(ctx context.Context, reference string) (Trade, error) {
	var t Trade
	return t, d.do(ctx, http.MethodGet, "/v1/trades/by-reference/"+url.PathEscape(reference), nil, &t)
}

func (d *Ditto) CloseTrade(ctx context.Context, reference string) (Fence, error) {
	var f Fence
	return f, d.do(ctx, http.MethodPost, "/v1/trades/by-reference/"+url.PathEscape(reference)+"/close", nil, &f)
}

func (d *Ditto) CreatePayout(ctx context.Context, req PayoutRequest) (Payout, error) {
	var p Payout
	return p, d.do(ctx, http.MethodPost, "/v1/payouts", req, &p)
}

func (d *Ditto) Payout(ctx context.Context, reference string) (Payout, error) {
	var p Payout
	return p, d.do(ctx, http.MethodGet, "/v1/payouts/by-reference/"+url.PathEscape(reference), nil, &p)
}

func (d *Ditto) CancelPayout(ctx context.Context, reference string) (Fence, error) {
	var f Fence
	return f, d.do(ctx, http.MethodPost, "/v1/payouts/by-reference/"+url.PathEscape(reference)+"/cancel", nil, &f)
}

func (d *Ditto) CreateStatement(ctx context.Context, after string) (Statement, error) {
	body := map[string]string{}
	if after != "" {
		body["after_statement_id"] = after
	}
	var s Statement
	return s, d.do(ctx, http.MethodPost, "/v1/statements", body, &s)
}

func (d *Ditto) StatementPage(ctx context.Context, id, cursor string) (Page, error) {
	var p Page
	return p, d.do(ctx, http.MethodGet, "/v1/statements/"+url.PathEscape(id)+"/pages?cursor="+url.QueryEscape(cursor), nil, &p)
}

func (d *Ditto) do(ctx context.Context, method, path string, body, into any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, d.baseURL+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+d.key)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	switch resp.StatusCode {
	case http.StatusOK, http.StatusCreated:
		return json.Unmarshal(data, into)
	case http.StatusNotFound:
		return ErrNotVisible
	case http.StatusConflict:
		return ErrConflict
	case http.StatusTooManyRequests:
		retryAfter := defaultRetryAfter
		if seconds, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil {
			retryAfter = time.Duration(max(seconds, 1)) * time.Second
		}
		return &RateLimitedError{RetryAfter: retryAfter}
	}
	return fmt.Errorf("%s %s answered %s: %s", method, path, resp.Status, data)
}
