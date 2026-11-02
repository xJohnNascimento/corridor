package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var (
	ErrQuoteExpired      = errors.New("the quote has expired")
	ErrTransitionRefused = errors.New("the row is not in a state this transition starts from")
)

type QuoteUsedError struct {
	PaymentID uuid.UUID
}

func (e *QuoteUsedError) Error() string {
	return fmt.Sprintf("payment %s already used the quote", e.PaymentID)
}

type Beneficiary struct {
	Name     string `json:"name"`
	Account  string `json:"account"`
	BankCode string `json:"bank_code"`
	Country  string `json:"country"`
}

type AdmitParams struct {
	CustomerID     string
	RequestKey     string
	RequestHash    []byte
	QuoteID        uuid.UUID
	Beneficiary    Beneficiary
	RemittanceInfo string
	Policy         any
}

type Payment struct {
	ID                   uuid.UUID
	CustomerID           string
	QuoteID              uuid.UUID
	BeneficiaryID        uuid.UUID
	PayoutID             uuid.UUID
	Status               string
	OutcomeReason        string
	ReviewOpen           bool
	ReviewReason         string
	ReviewRevision       int
	WorkflowStartPending bool
	Policy               json.RawMessage
	AcceptedAt           time.Time
	UpdatedAt            time.Time
	Payout               Payout
	Quote                Quote
}

type Payout struct {
	ID                  uuid.UUID
	Amount              int64
	Currency            string
	Status              string
	Attempt             int
	ClearingAccountID   string
	ReservationID       string
	ReleaseAt           *time.Time
	CancelAcceptedAt    *time.Time
	SubmissionStartedAt *time.Time
	ProviderPayoutID    string
	ReturnedAmount      int64
}

// Admit stores the payment, its PLANNED payout and its first timeline event in one transaction,
// after locking the quote and checking its owner, its expiry and that no payment used it yet.
func (s *Store) Admit(ctx context.Context, p AdmitParams) (Payment, bool, error) {
	if existing, err := s.paymentByKey(ctx, p.CustomerID, p.RequestKey); err == nil {
		return replayed(existing, p.RequestHash)
	} else if !errors.Is(err, ErrNotFound) {
		return Payment{}, false, err
	}
	paymentID, err := uuid.NewV7()
	if err != nil {
		return Payment{}, false, err
	}
	payoutID, err := uuid.NewV7()
	if err != nil {
		return Payment{}, false, err
	}
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var (
			expired  bool
			buy      int64
			currency string
		)
		err := tx.QueryRow(ctx, "select buy, buy_currency from fx.quote where id = $1 and customer_id = $2 for update", p.QuoteID, p.CustomerID).Scan(&buy, &currency)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		// A separate statement: one that locks and reads the clock together evaluates the clock
		// before it waits for the lock, so a quote could expire while the caller queued.
		if err := tx.QueryRow(ctx, "select clock_timestamp() > expires_at from fx.quote where id = $1", p.QuoteID).Scan(&expired); err != nil {
			return err
		}
		var used uuid.UUID
		err = tx.QueryRow(ctx, "select id from fx.payment where quote_id = $1", p.QuoteID).Scan(&used)
		if err == nil {
			return &QuoteUsedError{PaymentID: used}
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if expired {
			return ErrQuoteExpired
		}

		beneficiaryID, err := upsertBeneficiary(ctx, tx, p.CustomerID, p.Beneficiary)
		if err != nil {
			return err
		}
		policy, err := json.Marshal(p.Policy)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `
			insert into fx.payment (id, customer_id, request_key, request_hash, quote_id, beneficiary_id, payout_id,
			                        remittance_info, status, policy)
			values ($1, $2, $3, $4, $5, $6, $7, nullif($8, ''), 'ADMITTED', $9)`,
			paymentID, p.CustomerID, p.RequestKey, p.RequestHash, p.QuoteID, beneficiaryID, payoutID, p.RemittanceInfo, policy,
		)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `
			insert into fx.payout (id, payment_id, amount, currency, beneficiary_id) values ($1, $2, $3, $4, $5)`,
			payoutID, paymentID, buy, currency, beneficiaryID,
		)
		if err != nil {
			return err
		}
		return AddEvent(ctx, tx, paymentID, "ADMITTED", "", "CUSTOMER", "admitted")
	})
	var (
		unique *pgconn.PgError
		used   *QuoteUsedError
	)
	if errors.As(err, &used) || errors.As(err, &unique) && unique.Code == "23505" {
		existing, readErr := s.paymentByKey(ctx, p.CustomerID, p.RequestKey)
		if errors.Is(readErr, ErrNotFound) && used != nil {
			return Payment{}, false, used
		}
		if readErr != nil {
			return Payment{}, false, readErr
		}
		return replayed(existing, p.RequestHash)
	}
	if err != nil {
		return Payment{}, false, err
	}
	payment, err := s.Payment(ctx, paymentID)
	return payment, false, err
}

type storedPayment struct {
	Payment
	requestKey  string
	requestHash []byte
}

func replayed(existing storedPayment, hash []byte) (Payment, bool, error) {
	if !slices.Equal(existing.requestHash, hash) {
		return Payment{}, false, ErrIdempotencyMismatch
	}
	return existing.Payment, true, nil
}

func upsertBeneficiary(ctx context.Context, tx pgx.Tx, customer string, b Beneficiary) (uuid.UUID, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return uuid.Nil, err
	}
	hash := Hash(b)
	_, err = tx.Exec(ctx, `
		insert into fx.beneficiary (id, customer_id, name, account, bank_code, country, details_hash)
		values ($1, $2, $3, $4, $5, $6, $7)
		on conflict (customer_id, details_hash) do nothing`,
		id, customer, b.Name, b.Account, b.BankCode, b.Country, hash,
	)
	if err != nil {
		return uuid.Nil, err
	}
	err = tx.QueryRow(ctx, "select id from fx.beneficiary where customer_id = $1 and details_hash = $2", customer, hash).Scan(&id)
	return id, err
}

func AddEvent(ctx context.Context, tx pgx.Tx, payment uuid.UUID, kind, detail, visibility, key string) error {
	_, err := tx.Exec(ctx, `
		insert into fx.payment_event (payment_id, kind, detail, visibility, effect_key) values ($1, $2, $3, $4, $5)
		on conflict (payment_id, effect_key) do nothing`,
		payment, kind, detail, visibility, key,
	)
	return err
}

const paymentColumns = `p.id, p.customer_id, p.quote_id, p.beneficiary_id, p.payout_id, p.status, coalesce(p.outcome_reason, ''),
	p.review_open, coalesce(p.review_reason, ''), p.review_revision, p.workflow_start_pending, p.policy, p.accepted_at, p.updated_at,
	p.request_key, p.request_hash,
	o.id, o.amount, o.currency, o.status, o.attempt, coalesce(o.clearing_account_id::text, ''), coalesce(o.reservation_id::text, ''),
	o.release_at, o.cancel_accepted_at, o.submission_started_at, coalesce(o.provider_payout_id, ''), coalesce(o.returned_amount, 0)`

const paymentFrom = ` from fx.payment p join fx.payout o on o.payment_id = p.id`

func scanPayment(row pgx.Row) (storedPayment, error) {
	var p storedPayment
	o := &p.Payout
	err := row.Scan(&p.ID, &p.CustomerID, &p.QuoteID, &p.BeneficiaryID, &p.PayoutID, &p.Status, &p.OutcomeReason,
		&p.ReviewOpen, &p.ReviewReason, &p.ReviewRevision, &p.WorkflowStartPending, &p.Policy, &p.AcceptedAt, &p.UpdatedAt,
		&p.requestKey, &p.requestHash,
		&o.ID, &o.Amount, &o.Currency, &o.Status, &o.Attempt, &o.ClearingAccountID, &o.ReservationID,
		&o.ReleaseAt, &o.CancelAcceptedAt, &o.SubmissionStartedAt, &o.ProviderPayoutID, &o.ReturnedAmount)
	if errors.Is(err, pgx.ErrNoRows) {
		return storedPayment{}, ErrNotFound
	}
	return p, err
}

func (s *Store) paymentByKey(ctx context.Context, customer, key string) (storedPayment, error) {
	return s.loadStored(ctx, s.pool.QueryRow(ctx, "select "+paymentColumns+paymentFrom+" where p.customer_id = $1 and p.request_key = $2", customer, key))
}

func (s *Store) loadStored(ctx context.Context, row pgx.Row) (storedPayment, error) {
	p, err := scanPayment(row)
	if err != nil {
		return p, err
	}
	p.Quote, err = scanQuote(s.pool.QueryRow(ctx, "select "+quoteColumns+" from fx.quote where id = $1", p.QuoteID))
	return p, err
}

func (s *Store) Payment(ctx context.Context, id uuid.UUID) (Payment, error) {
	p, err := s.loadStored(ctx, s.pool.QueryRow(ctx, "select "+paymentColumns+paymentFrom+" where p.id = $1", id))
	return p.Payment, err
}

// CustomerPayment reads a payment for its owner only; anyone else's is not found.
func (s *Store) CustomerPayment(ctx context.Context, id uuid.UUID, customer string) (Payment, error) {
	p, err := s.Payment(ctx, id)
	if err == nil && p.CustomerID != customer {
		return Payment{}, ErrNotFound
	}
	return p, err
}

type Event struct {
	Kind       string
	Detail     string
	OccurredAt time.Time
}

func (s *Store) Timeline(ctx context.Context, payment uuid.UUID, operator bool) ([]Event, error) {
	rows, err := s.pool.Query(ctx, `
		select kind, detail, occurred_at from fx.payment_event
		where payment_id = $1 and (visibility = 'CUSTOMER' or $2)
		order by id`,
		payment, operator,
	)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Event, error) {
		var e Event
		return e, row.Scan(&e.Kind, &e.Detail, &e.OccurredAt)
	})
}

func (s *Store) ClearStartPending(ctx context.Context, payment uuid.UUID) error {
	_, err := s.pool.Exec(ctx, "update fx.payment set workflow_start_pending = false, start_lease_until = null where id = $1", payment)
	return err
}

// LeasePendingStarts leases payments whose workflow start is still pending, committing the lease
// before the caller talks to Temporal, so any number of dispatchers can run. Rows never tried come
// first, so the ones that keep failing cannot starve the rest.
func (s *Store) LeasePendingStarts(ctx context.Context, limit int, lease time.Duration) ([]uuid.UUID, error) {
	rows, err := s.pool.Query(ctx, `
		update fx.payment set start_lease_until = now() + $2::interval
		where id in (
			select id from fx.payment
			where workflow_start_pending and (start_lease_until is null or start_lease_until < now())
			order by start_lease_until nulls first, accepted_at
			limit $1
			for update skip locked
		)
		returning id`,
		limit, lease.String(),
	)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
}

type Transition struct {
	Key     string
	From    []string
	To      string
	Outcome string
	Event   string
}

// Table says which row a transition moves: the payment itself or its payout.
type Table struct {
	name           string
	keyColumn      string
	visibility     string
	recordsOutcome bool
}

var (
	PaymentTable = Table{name: "payment", keyColumn: "id", visibility: "CUSTOMER", recordsOutcome: true}
	PayoutTable  = Table{name: "payout", keyColumn: "payment_id", visibility: "OPERATOR"}
)

func (s *Store) TransitionPayment(ctx context.Context, payment uuid.UUID, t Transition) error {
	return s.InTx(ctx, func(tx pgx.Tx) error {
		return TransitionIn(ctx, tx, PaymentTable, payment, t)
	})
}

// TransitionPayout runs set and then moves the payment's payout, in one transaction, so the row
// already carries the evidence and ids its new status requires.
func (s *Store) TransitionPayout(ctx context.Context, payment uuid.UUID, t Transition, set func(context.Context, pgx.Tx) error) error {
	return s.InTx(ctx, func(tx pgx.Tx) error {
		if set != nil {
			if err := set(ctx, tx); err != nil {
				return err
			}
		}
		return TransitionIn(ctx, tx, PayoutTable, payment, t)
	})
}

func (s *Store) InTx(ctx context.Context, fn func(pgx.Tx) error) error {
	return pgx.BeginFunc(ctx, s.pool, fn)
}

// TransitionIn moves a row from one of the expected statuses to the target under a compare-and-set.
// A retry of the same transition key that finds the target already written succeeds without
// writing, so a lost reply never turns into a failed transition.
func TransitionIn(ctx context.Context, tx pgx.Tx, table Table, payment uuid.UUID, t Transition) error {
	var status, lastKey string
	err := tx.QueryRow(ctx,
		"select status, coalesce(last_transition_key, '') from fx."+table.name+" where "+table.keyColumn+" = $1 for update", payment,
	).Scan(&status, &lastKey)
	if err != nil {
		return err
	}
	if status == t.To && lastKey == t.Key {
		return nil
	}
	if !slices.Contains(t.From, status) {
		return fmt.Errorf("%w: %s of payment %s is %s, transition %s expects %v", ErrTransitionRefused, table.name, payment, status, t.Key, t.From)
	}
	set := "status = $2, last_transition_key = $3, version = version + 1, updated_at = now()"
	args := []any{payment, t.To, t.Key}
	if table.recordsOutcome {
		set += ", outcome_reason = nullif($4, '')"
		args = append(args, t.Outcome)
	}
	_, err = tx.Exec(ctx, "update fx."+table.name+" set "+set+" where "+table.keyColumn+" = $1", args...)
	if err != nil || t.Event == "" {
		return err
	}
	return AddEvent(ctx, tx, payment, t.Event, t.Outcome, table.visibility, table.name+"/"+t.Key)
}

type PaymentSummary struct {
	ID           uuid.UUID
	Status       string
	Outcome      string
	Review       string
	PayoutStatus string
	StartPending bool
}

// PaymentSummaries lists a customer's payments in admission order, for the operators' CLI.
func (s *Store) PaymentSummaries(ctx context.Context, customer string) ([]PaymentSummary, error) {
	rows, err := s.pool.Query(ctx, `
		select p.id, p.status, coalesce(p.outcome_reason, ''), coalesce(p.review_reason, ''), o.status, p.workflow_start_pending
		from fx.payment p join fx.payout o on o.payment_id = p.id
		where p.customer_id = $1 order by p.accepted_at`,
		customer,
	)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (PaymentSummary, error) {
		var p PaymentSummary
		return p, row.Scan(&p.ID, &p.Status, &p.Outcome, &p.Review, &p.PayoutStatus, &p.StartPending)
	})
}
