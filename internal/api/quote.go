package api

import (
	"context"
	"errors"
	"fmt"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/timestamppb"

	corridorv1 "github.com/xJohnNascimento/corridor/api/corridor/v1"
	"github.com/xJohnNascimento/corridor/internal/money"
	"github.com/xJohnNascimento/corridor/internal/provider"
	"github.com/xJohnNascimento/corridor/internal/store"
)

type quoteRequest struct {
	Sell   string `json:"sell"`
	Buy    string `json:"buy"`
	Amount int64  `json:"amount"`
}

type quoteService struct {
	*Server
}

// CreateQuote claims a command, mints the quote id as the provider's client reference, prices the
// provider's quote and freezes it. A replay answers the command as it was settled; an interrupted
// call resumes with the same client reference, so a lost answer never creates a second provider
// quote.
func (s quoteService) CreateQuote(ctx context.Context, req *connect.Request[corridorv1.CreateQuoteRequest]) (*connect.Response[corridorv1.CreateQuoteResponse], error) {
	actor := actorFrom(ctx)
	key, err := idempotencyKey(req.Header())
	if err != nil {
		return nil, err
	}
	m := req.Msg
	sell, sellErr := money.ParseCurrency(m.SellCurrency)
	buy, buyErr := money.ParseCurrency(m.BuyCurrency)
	amount, amountErr := money.ParseAmount(m.SellAmount)
	if err := errors.Join(sellErr, buyErr, amountErr); err != nil {
		return nil, failure(corridorv1.ErrorReason_ERROR_REASON_VALIDATION_FAILED, err.Error())
	}
	if !s.cfg.Enabled(string(sell), string(buy)) {
		return nil, failure(corridorv1.ErrorReason_ERROR_REASON_CORRIDOR_DISABLED, fmt.Sprintf("%s to %s is not enabled", sell, buy))
	}
	if amount < s.cfg.Limits.MinSell || amount > s.cfg.Limits.MaxSell {
		return nil, failure(corridorv1.ErrorReason_ERROR_REASON_AMOUNT_OUT_OF_RANGE,
			fmt.Sprintf("the sold amount must be between %d and %d minor units", s.cfg.Limits.MinSell, s.cfg.Limits.MaxSell))
	}
	// The budget pays for pricing work, not for the replay of a command already settled. It is
	// taken before the claim so a refusal leaves no command behind, and a retry of the same key
	// still pays for itself.
	state, err := s.store.CommandState(ctx, actor.ID, key)
	if err != nil {
		return nil, unavailable(ctx, err)
	}
	if state != "APPLIED" && state != "REJECTED" {
		wait, err := s.store.TakeQuoteToken(ctx, actor.ID, s.cfg.Quotes.PerSecond, s.cfg.Quotes.Burst)
		if err != nil {
			return nil, unavailable(ctx, err)
		}
		if wait > 0 {
			return nil, rateLimited(wait)
		}
	}
	cmd, err := s.claim(ctx, actor, key, "CREATE_QUOTE", "", quoteRequest{Sell: string(sell), Buy: string(buy), Amount: amount}, false)
	if err != nil {
		return nil, err
	}
	if cmd.State == "RECEIVED" {
		if err := s.settleQuote(ctx, actor.ID, cmd.ID, string(sell), string(buy), amount); err != nil {
			return nil, err
		}
		// A concurrent request with the same key may have settled the command first.
		if cmd, err = s.store.Command(ctx, cmd.ID); err != nil {
			return nil, unavailable(ctx, err)
		}
	}
	if cmd.State == "REJECTED" {
		return nil, failure(corridorv1.ErrorReason_ERROR_REASON_QUOTE_EXPIRED,
			"the provider's quote for this Idempotency-Key expires too soon to book; request a new quote with a new key")
	}
	q, err := s.store.QuoteByCommand(ctx, cmd.ID)
	if err != nil {
		return nil, unavailable(ctx, err)
	}
	return connect.NewResponse(&corridorv1.CreateQuoteResponse{Quote: quoteMessage(q)}), nil
}

// errUnbookable is a provider quote that expires too soon to book. The provider answers the same
// quote for the same client reference, so no retry of the key could ever book it.
var errUnbookable = errors.New("the provider quote expires too soon to book")

// settleQuote freezes the priced quote, or refuses the command for good when the provider's quote
// is unbookable.
func (s quoteService) settleQuote(ctx context.Context, customer string, command uuid.UUID, sell, buy string, amount int64) error {
	q, err := s.priceQuote(ctx, customer, command.String(), sell, buy, amount)
	if errors.Is(err, errUnbookable) {
		return storeError(ctx, s.store.RejectCommand(ctx, command, "QUOTE_EXPIRED"))
	}
	if err != nil {
		return err
	}
	q.CommandID = command
	_, err = s.store.FreezeQuote(ctx, q, s.cfg.Quotes.TTL)
	return storeError(ctx, err)
}

func storeError(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	return unavailable(ctx, err)
}

func (s quoteService) priceQuote(ctx context.Context, customer, id, sell, buy string, amount int64) (store.Quote, error) {
	schedule := s.cfg.Schedule()
	_, net, err := schedule.Net(amount)
	if err != nil {
		return store.Quote{}, failure(corridorv1.ErrorReason_ERROR_REASON_AMOUNT_OUT_OF_RANGE, err.Error())
	}
	pq, err := s.provider.CreateQuote(ctx, id, sell, buy, net)
	var limited *provider.RateLimitedError
	if errors.As(err, &limited) {
		return store.Quote{}, rateLimited(limited.RetryAfter)
	}
	if err != nil {
		return store.Quote{}, unavailable(ctx, err)
	}
	rate, rateErr := money.ParseAmount(pq.RateE8)
	providerBuy, buyErr := money.ParseAmount(pq.BuyAmount)
	if err := errors.Join(rateErr, buyErr); err != nil {
		return store.Quote{}, unavailable(ctx, fmt.Errorf("the provider quote %s is not numeric: %w", pq.ID, err))
	}
	if pq.SellCurrency != sell || pq.BuyCurrency != buy || pq.SellAmount != money.Format(net) {
		return store.Quote{}, unavailable(ctx, fmt.Errorf("the provider quote %s does not match the request", pq.ID))
	}
	priced, err := schedule.Price(amount, rate, providerBuy)
	if err != nil {
		return store.Quote{}, unavailable(ctx, err)
	}
	now, err := s.store.DatabaseNow(ctx)
	if err != nil {
		return store.Quote{}, unavailable(ctx, err)
	}
	if pq.ExpiresAt.Before(now.Add(s.cfg.Quotes.TTL + s.cfg.Quotes.BookingMargin)) {
		return store.Quote{}, fmt.Errorf("provider quote %s expires at %s: %w", pq.ID, pq.ExpiresAt, errUnbookable)
	}
	return store.Quote{
		CustomerID: customer, SellCurrency: sell, BuyCurrency: buy, SellAmount: priced.Sell, Fee: priced.Fee, NetSell: priced.NetSell,
		ProviderRateE8: priced.ProviderRateE8, CustomerRateE8: priced.CustomerRateE8, ProviderBuy: priced.ProviderBuy,
		Buy: priced.Buy, Spread: priced.Spread, PolicyVersion: s.policy.Version, ProviderQuoteID: pq.ID, ProviderExpiresAt: pq.ExpiresAt,
	}, nil
}

func quoteMessage(q store.Quote) *corridorv1.Quote {
	return &corridorv1.Quote{
		Id:             q.ID.String(),
		Sell:           &corridorv1.Money{Currency: q.SellCurrency, Amount: money.Format(q.SellAmount)},
		Fee:            &corridorv1.Money{Currency: q.SellCurrency, Amount: money.Format(q.Fee)},
		NetSell:        &corridorv1.Money{Currency: q.SellCurrency, Amount: money.Format(q.NetSell)},
		Buy:            &corridorv1.Money{Currency: q.BuyCurrency, Amount: money.Format(q.Buy)},
		CustomerRateE8: money.Format(q.CustomerRateE8),
		ExpiresAt:      timestamppb.New(q.ExpiresAt),
		CreatedAt:      timestamppb.New(q.CreatedAt),
	}
}
