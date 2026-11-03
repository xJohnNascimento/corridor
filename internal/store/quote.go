package store

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type Quote struct {
	ID                uuid.UUID
	CustomerID        string
	CommandID         uuid.UUID
	SellCurrency      string
	BuyCurrency       string
	SellAmount        int64
	Fee               int64
	NetSell           int64
	ProviderRateE8    int64
	CustomerRateE8    int64
	ProviderBuy       int64
	Buy               int64
	Spread            int64
	PolicyVersion     string
	ProviderQuoteID   string
	ProviderExpiresAt time.Time
	ExpiresAt         time.Time
	CreatedAt         time.Time
}

const quoteColumns = `id, customer_id, command_id, sell_currency, buy_currency, sell_amount, fee, net_sell, provider_rate_e8,
	customer_rate_e8, provider_buy, buy, spread, policy_version, provider_quote_id, provider_expires_at, expires_at, created_at`

func scanQuote(row pgx.Row) (Quote, error) {
	var q Quote
	err := row.Scan(&q.ID, &q.CustomerID, &q.CommandID, &q.SellCurrency, &q.BuyCurrency, &q.SellAmount, &q.Fee, &q.NetSell,
		&q.ProviderRateE8, &q.CustomerRateE8, &q.ProviderBuy, &q.Buy, &q.Spread, &q.PolicyVersion, &q.ProviderQuoteID,
		&q.ProviderExpiresAt, &q.ExpiresAt, &q.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Quote{}, ErrNotFound
	}
	return q, err
}

// FreezeQuote stores the priced quote under its command's id and applies the command in one
// transaction. Its expiry is set from the database clock. Concurrent calls for one command all
// return the quote the first one stored: the insert yields to a conflict on any of the quote's
// unique keys, since a conflict target names only one and two inserts can collide on another.
func (s *Store) FreezeQuote(ctx context.Context, q Quote, ttl time.Duration) (Quote, error) {
	var frozen Quote
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			insert into fx.quote (id, customer_id, command_id, sell_currency, buy_currency, sell_amount, fee, net_sell,
			                      provider_rate_e8, customer_rate_e8, provider_buy, buy, spread, policy_version,
			                      provider_quote_id, provider_expires_at, expires_at)
			values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, now() + $17::interval)
			on conflict do nothing`,
			q.CommandID, q.CustomerID, q.CommandID, q.SellCurrency, q.BuyCurrency, q.SellAmount, q.Fee, q.NetSell,
			q.ProviderRateE8, q.CustomerRateE8, q.ProviderBuy, q.Buy, q.Spread, q.PolicyVersion,
			q.ProviderQuoteID, q.ProviderExpiresAt, ttl.String(),
		)
		if err != nil {
			return err
		}
		frozen, err = scanQuote(tx.QueryRow(ctx, "select "+quoteColumns+" from fx.quote where command_id = $1", q.CommandID))
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `
			update fx.product_command set state = 'APPLIED', effect_ids = array[$2::text], accepted_at = now(), applied_at = now()
			where id = $1 and state = 'RECEIVED'`,
			q.CommandID, frozen.ID.String(),
		)
		return err
	})
	return frozen, err
}

func (s *Store) QuoteByCommand(ctx context.Context, commandID uuid.UUID) (Quote, error) {
	return scanQuote(s.pool.QueryRow(ctx, "select "+quoteColumns+" from fx.quote where command_id = $1", commandID))
}

func (s *Store) Quote(ctx context.Context, id uuid.UUID, customer string) (Quote, error) {
	return scanQuote(s.pool.QueryRow(ctx, "select "+quoteColumns+" from fx.quote where id = $1 and customer_id = $2", id, customer))
}

func (s *Store) DatabaseNow(ctx context.Context) (time.Time, error) {
	var now time.Time
	err := s.pool.QueryRow(ctx, "select now()").Scan(&now)
	return now, err
}
