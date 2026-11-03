package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Querier is a pool or a transaction.
type Querier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

type Evidence struct {
	Source    string
	Operation string
	Reference string
	Outcome   string
	Currency  string
	Amount    int64
	Sequence  int
	Payload   any
}

// RecordEvidence stores a provider observation once per source identity and returns its id and
// digest. Evidence is append-only: money moves only on evidence that was stored first. The identity
// carries the provider's sequence, so two events of one payout never collapse, and a digest prefix,
// so a changed body under the same reference is kept next to the first.
func RecordEvidence(ctx context.Context, q Querier, e Evidence) (string, string, error) {
	payload, err := json.Marshal(e.Payload)
	if err != nil {
		return "", "", err
	}
	digest := sha256.Sum256(payload)
	identity := fmt.Sprintf("%s:%s:%s:%s:%d:%x", e.Source, e.Operation, e.Reference, e.Outcome, e.Sequence, digest[:8])
	id, err := uuid.NewV7()
	if err != nil {
		return "", "", err
	}
	var stored string
	err = q.QueryRow(ctx, `
		with inserted as (
			insert into fx.provider_evidence (id, source, operation, reference, outcome, currency, amount, sequence, payload, digest, source_identity)
			values ($1, $2, $3, $4, $5, nullif($6, ''), nullif($7::bigint, 0), nullif($8, 0), $9, $10, $11)
			on conflict (source_identity) do nothing
			returning id
		)
		select id::text from inserted
		union all
		select id::text from fx.provider_evidence where source_identity = $11
		limit 1`,
		id, e.Source, e.Operation, e.Reference, e.Outcome, e.Currency, e.Amount, e.Sequence, payload, digest[:], identity,
	).Scan(&stored)
	return stored, hex.EncodeToString(digest[:]), err
}

// EvidenceAmount reads the currency and amount a stored observation recorded.
func (s *Store) EvidenceAmount(ctx context.Context, id uuid.UUID) (string, int64, error) {
	var (
		currency string
		amount   int64
	)
	err := s.pool.QueryRow(ctx, "select coalesce(currency, ''), coalesce(amount, 0) from fx.provider_evidence where id = $1", id).Scan(&currency, &amount)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", 0, ErrNotFound
	}
	return currency, amount, err
}
