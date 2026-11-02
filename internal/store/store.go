package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrNotFound            = errors.New("not found")
	ErrIdempotencyMismatch = errors.New("the key was already used with a different request")
)

type Store struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

func (s *Store) Pool() *pgxpool.Pool {
	return s.pool
}

// Hash is the canonical hash of a request: its fields in a fixed struct order, so field order in
// the JSON a client sent never changes it.
func Hash(request any) []byte {
	encoded, err := json.Marshal(request)
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(encoded)
	return sum[:]
}

func KeyHash(key string) []byte {
	sum := sha256.Sum256([]byte(key))
	return sum[:]
}

func (s *Store) UpsertCustomer(ctx context.Context, id, name string, keyHash []byte) error {
	_, err := s.pool.Exec(ctx, `
		insert into fx.customer (id, name, key_hash) values ($1, $2, $3)
		on conflict (id) do update set name = excluded.name, key_hash = excluded.key_hash`,
		id, name, keyHash,
	)
	return err
}

func (s *Store) SetCustomerAccount(ctx context.Context, customer, currency, ledgerAccountID string) error {
	_, err := s.pool.Exec(ctx, `
		insert into fx.customer_account (customer_id, currency, ledger_account_id) values ($1, $2, $3)
		on conflict (customer_id, currency) do nothing`,
		customer, currency, ledgerAccountID,
	)
	return err
}

func (s *Store) CustomerAccount(ctx context.Context, customer, currency string) (string, error) {
	var id string
	err := s.pool.QueryRow(ctx, "select ledger_account_id from fx.customer_account where customer_id = $1 and currency = $2", customer, currency).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	return id, err
}

func (s *Store) UpsertOperator(ctx context.Context, id, name string, keyHash []byte) error {
	_, err := s.pool.Exec(ctx, `
		insert into fx.operator (id, name, key_hash) values ($1, $2, $3)
		on conflict (id) do update set name = excluded.name, key_hash = excluded.key_hash`,
		id, name, keyHash,
	)
	return err
}

func (s *Store) CustomerByKey(ctx context.Context, keyHash []byte) (string, error) {
	var id string
	err := s.pool.QueryRow(ctx, "select id from fx.customer where key_hash = $1 and status = 'ACTIVE'", keyHash).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	return id, err
}

func (s *Store) OperatorByKey(ctx context.Context, keyHash []byte) (string, error) {
	var id string
	err := s.pool.QueryRow(ctx, "select id from fx.operator where key_hash = $1", keyHash).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	return id, err
}

// TakeQuoteToken spends one token from the customer's bucket in a single locked statement shared
// by every API replica, and says how long to wait when none is left.
func (s *Store) TakeQuoteToken(ctx context.Context, customer string, perSecond float64, burst int) (time.Duration, error) {
	var (
		admitted  bool
		available float64
	)
	_, err := s.pool.Exec(ctx, `
		insert into fx.rate_bucket (customer_id, tokens, updated_at) values ($1, $2, clock_timestamp())
		on conflict (customer_id) do nothing`,
		customer, burst,
	)
	if err != nil {
		return 0, err
	}
	err = s.pool.QueryRow(ctx, `
		with clock as (select clock_timestamp() as now),
		current as (
			select b.customer_id, least($3::numeric, b.tokens + extract(epoch from clock.now - b.updated_at)::numeric * $2) as available, clock.now
			from fx.rate_bucket b, clock
			where b.customer_id = $1
			for update of b
		)
		update fx.rate_bucket b
		set tokens = case when c.available >= 1 then c.available - 1 else c.available end, updated_at = c.now
		from current c
		where b.customer_id = c.customer_id
		returning c.available >= 1, c.available::float8`,
		customer, perSecond, burst,
	).Scan(&admitted, &available)
	if err != nil || admitted {
		return 0, err
	}
	return time.Duration(math.Ceil(math.Max((1-available)/perSecond, 1))) * time.Second, nil
}

type Command struct {
	ID         uuid.UUID
	ActorKind  string
	ActorID    string
	RequestKey string
	Operation  string
	SubjectID  string
	State      string
	Reason     string
	EffectIDs  []string
	ReceivedAt time.Time
	AcceptedAt *time.Time
	AppliedAt  *time.Time
}

type CommandClaim struct {
	ActorKind  string
	ActorID    string
	RequestKey string
	Operation  string
	SubjectID  string
	Request    any
}

const commandColumns = `id, actor_kind, actor_id, request_key, operation, coalesce(subject_id, ''), state,
	coalesce(reason, ''), effect_ids, received_at, accepted_at, applied_at`

func scanCommand(row pgx.Row, extra ...any) (Command, error) {
	var c Command
	dest := append([]any{&c.ID, &c.ActorKind, &c.ActorID, &c.RequestKey, &c.Operation, &c.SubjectID, &c.State,
		&c.Reason, &c.EffectIDs, &c.ReceivedAt, &c.AcceptedAt, &c.AppliedAt}, extra...)
	return c, row.Scan(dest...)
}

// ClaimCommand stores a command under its actor and Idempotency-Key, or returns the one already
// stored. The read after a conflict is a new statement, so it sees the winner's commit.
func (s *Store) ClaimCommand(ctx context.Context, c CommandClaim, deliver bool) (Command, bool, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return Command{}, false, err
	}
	hash := Hash(c.Request)
	body, err := json.Marshal(c.Request)
	if err != nil {
		return Command{}, false, err
	}
	tag, err := s.pool.Exec(ctx, `
		insert into fx.product_command (id, actor_kind, actor_id, request_key, operation, subject_id, request_hash, request, delivery_pending)
		values ($1, $2, $3, $4, $5, nullif($6, ''), $7, $8, $9)
		on conflict (actor_id, request_key) do nothing`,
		id, c.ActorKind, c.ActorID, c.RequestKey, c.Operation, c.SubjectID, hash, body, deliver,
	)
	if err != nil {
		return Command{}, false, err
	}
	var storedHash []byte
	cmd, err := scanCommand(s.pool.QueryRow(ctx,
		"select "+commandColumns+", request_hash from fx.product_command where actor_id = $1 and request_key = $2", c.ActorID, c.RequestKey,
	), &storedHash)
	if err != nil {
		return Command{}, false, err
	}
	if cmd.Operation != c.Operation || !bytes.Equal(storedHash, hash) {
		return Command{}, false, ErrIdempotencyMismatch
	}
	return cmd, tag.RowsAffected() == 1, nil
}

// CommandState is the state of the command stored under the actor's Idempotency-Key, or "" when
// there is none. It does not claim anything.
func (s *Store) CommandState(ctx context.Context, actorID, requestKey string) (string, error) {
	var state string
	err := s.pool.QueryRow(ctx, "select state from fx.product_command where actor_id = $1 and request_key = $2", actorID, requestKey).Scan(&state)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return state, err
}

func (s *Store) Command(ctx context.Context, id uuid.UUID) (Command, error) {
	c, err := scanCommand(s.pool.QueryRow(ctx, "select "+commandColumns+" from fx.product_command where id = $1", id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Command{}, ErrNotFound
	}
	return c, err
}

type execer interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}

// RejectCommand settles a command that was refused before it had any effect to store.
func (s *Store) RejectCommand(ctx context.Context, id uuid.UUID, reason string) error {
	return settleIn(ctx, s.pool, id, "REJECTED", reason)
}

// applyIn and settleIn finish a command inside the transaction of the effect it describes, so a
// crash can never leave the effect stored with its command still RECEIVED.
func applyIn(ctx context.Context, tx execer, id uuid.UUID, effects ...string) error {
	_, err := tx.Exec(ctx, `
		update fx.product_command
		set state = 'APPLIED', effect_ids = $2, accepted_at = coalesce(accepted_at, now()), applied_at = now(), delivery_pending = false
		where id = $1 and state in ('RECEIVED', 'ACCEPTED')`,
		id, effects,
	)
	return err
}

func settleIn(ctx context.Context, tx execer, id uuid.UUID, state, reason string) error {
	_, err := tx.Exec(ctx, `
		update fx.product_command set state = $2, reason = nullif($3, ''), delivery_pending = false where id = $1 and state = 'RECEIVED'`,
		id, state, reason,
	)
	return err
}

// RecordIntent stores what Corridor is about to ask the ledger before it asks, so an unresolved
// call is visible and replayable with the same reference and body.
func (s *Store) RecordIntent(ctx context.Context, namespace, reference, operation string, payment uuid.UUID, request any) error {
	body, err := json.Marshal(request)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `
		insert into fx.ledger_intent (namespace, reference, operation, payment_id, request, request_hash)
		values ($1, $2, $3, $4, $5, $6)
		on conflict (namespace, reference) do nothing`,
		namespace, reference, operation, payment, body, Hash(request),
	)
	return err
}

func (s *Store) ObserveIntent(ctx context.Context, namespace, reference string, observed any) error {
	body, err := json.Marshal(observed)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, "update fx.ledger_intent set observed = $3, observed_at = now() where namespace = $1 and reference = $2",
		namespace, reference, body)
	return err
}

// ReceiveWebhook stores an event once by (provider, event id). The same id with another payload is
// kept as a CONFLICT row next to the first, never in its place.
func (s *Store) ReceiveWebhook(ctx context.Context, provider, eventID, reference string, payload []byte) (bool, error) {
	hash := sha256.Sum256(payload)
	tag, err := s.pool.Exec(ctx, `
		insert into fx.webhook_inbox (provider, event_id, payload_hash, payload, reference)
		values ($1, $2, $3, $4, nullif($5, ''))
		on conflict (provider, event_id) do nothing`,
		provider, eventID, hash[:], payload, reference,
	)
	if err != nil || tag.RowsAffected() == 1 {
		return false, err
	}
	var stored []byte
	if err := s.pool.QueryRow(ctx, "select payload_hash from fx.webhook_inbox where provider = $1 and event_id = $2", provider, eventID).Scan(&stored); err != nil {
		return false, err
	}
	if bytes.Equal(stored, hash[:]) {
		return false, nil
	}
	_, err = s.pool.Exec(ctx, `
		insert into fx.webhook_inbox (provider, event_id, payload_hash, payload, reference, status)
		values ($1, $2, $3, $4, nullif($5, ''), 'CONFLICT')
		on conflict do nothing`,
		provider, fmt.Sprintf("%s:conflict:%x", eventID, hash[:8]), hash[:], payload, reference,
	)
	return true, err
}

func (s *Store) ProposalPayment(ctx context.Context, proposal uuid.UUID) (uuid.UUID, error) {
	var payment uuid.UUID
	err := s.pool.QueryRow(ctx, "select payment_id from fx.resolution_proposal where id = $1", proposal).Scan(&payment)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, ErrNotFound
	}
	return payment, err
}

func (s *Store) ProposalDigest(ctx context.Context, proposal uuid.UUID) (string, error) {
	var digest string
	err := s.pool.QueryRow(ctx, "select encode(digest, 'hex') from fx.resolution_proposal where id = $1", proposal).Scan(&digest)
	return digest, err
}

// Snapshot runs fn in one repeatable-read, read-only transaction, so every read sees the same cut.
func (s *Store) Snapshot(ctx context.Context, fn func(pgx.Tx) error) error {
	return pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, fn)
}
