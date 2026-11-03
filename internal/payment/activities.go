package payment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.temporal.io/sdk/temporal"

	"github.com/xJohnNascimento/corridor/internal/config"
	"github.com/xJohnNascimento/corridor/internal/failpoint"
	"github.com/xJohnNascimento/corridor/internal/ledger"
	"github.com/xJohnNascimento/corridor/internal/provider"
	"github.com/xJohnNascimento/corridor/internal/store"
)

// failureAccountClosed is the ISO 20022 return code AC04, closed account number.
const failureAccountClosed = "AC04"

// Activities are the payment's side effects. Each uses its stable business reference on every
// attempt, because an effect may have happened even when an attempt timed out.
type Activities struct {
	Store    *store.Store
	Ledger   Ledger
	Provider provider.Provider
	Points   *failpoint.Points
}

// Ledger is what the payment flow needs from the ledger adapter.
type Ledger interface {
	ProviderCash(currency string) string
	EnsureClearing(ctx context.Context, payment uuid.UUID, currency string) (string, error)
	ReserveConversion(ctx context.Context, c ledger.Conversion) (ledger.Reservation, error)
	CommitConversion(ctx context.Context, c ledger.Conversion, reservationID string, delivered int64) (string, error)
	ReleaseConversion(ctx context.Context, payment uuid.UUID, reservationID string) error
	ReservePayout(ctx context.Context, p ledger.Payout, attempt int) (ledger.Reservation, error)
	CommitPayout(ctx context.Context, p ledger.Payout, reservationID string) (string, error)
	ReleasePayout(ctx context.Context, p ledger.Payout, reservationID string) (string, error)
	ReturnUnreservedPayout(ctx context.Context, p ledger.Payout) (string, error)
}

type terms struct {
	payment       store.Payment
	conversion    ledger.Conversion
	payout        ledger.Payout
	reservationID string
	policy        config.Policy
}

func (a *Activities) terms(ctx context.Context, paymentID string) (terms, error) {
	id, err := parseID(paymentID)
	if err != nil {
		return terms{}, err
	}
	p, err := a.Store.Payment(ctx, id)
	if err != nil {
		return terms{}, err
	}
	sellFunds, err := a.Store.CustomerAccount(ctx, p.CustomerID, p.Quote.SellCurrency)
	if err != nil {
		return terms{}, err
	}
	buyFunds, err := a.Store.CustomerAccount(ctx, p.CustomerID, p.Quote.BuyCurrency)
	if err != nil {
		return terms{}, err
	}
	var policy config.Policy
	if err := json.Unmarshal(p.Policy, &policy); err != nil {
		return terms{}, err
	}
	var reservation string
	err = a.Store.Pool().QueryRow(ctx, "select coalesce(reservation_id::text, '') from fx.trade where payment_id = $1", id).Scan(&reservation)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return terms{}, err
	}
	q := p.Quote
	return terms{
		payment: p,
		conversion: ledger.Conversion{
			PaymentID: id, CustomerFunds: sellFunds, Clearing: p.Payout.ClearingAccountID, SellCurrency: q.SellCurrency, BuyCurrency: q.BuyCurrency,
			Sell: q.SellAmount, Fee: q.Fee, Net: q.NetSell, ProviderBuy: q.ProviderBuy, Buy: q.Buy, Spread: q.Spread,
		},
		payout: ledger.Payout{
			PaymentID: id, PayoutID: p.Payout.ID, Clearing: p.Payout.ClearingAccountID, CustomerFunds: buyFunds,
			Currency: p.Payout.Currency, Amount: p.Payout.Amount,
		},
		reservationID: reservation,
		policy:        policy,
	}, nil
}

func (a *Activities) Load(ctx context.Context, paymentID string) (Snapshot, error) {
	var (
		s                           Snapshot
		paymentStatus, payoutStatus string
	)
	err := a.Store.Pool().QueryRow(ctx, `
		select p.status, o.status, o.attempt, coalesce(t.status, ''), coalesce(o.cancel_command_id::text, ''),
		       coalesce(p.review_reason, ''), o.release_at, coalesce(t.delivered_amount, 0), t.submission_started_at is not null
		from fx.payment p
		join fx.payout o on o.payment_id = p.id
		left join fx.trade t on t.payment_id = p.id
		where p.id = $1`,
		paymentID,
	).Scan(&paymentStatus, &payoutStatus, &s.Attempt, &s.TradeStatus, &s.CancelCommandID, &s.ReviewReason, &s.ReleaseAt, &s.DeliveredAmount, &s.SubmissionStarted)
	s.PaymentStatus, s.PayoutStatus = PaymentStatus(paymentStatus), PayoutStatus(payoutStatus)
	return s, err
}

// ReserveConversion reserves the customer's funds and the provider cash being sold, with the full
// settlement scope. A refusal is final for this payment and ends it before any trade.
func (a *Activities) ReserveConversion(ctx context.Context, paymentID string) (Reserved, error) {
	t, err := a.terms(ctx, paymentID)
	if err != nil {
		return Reserved{}, err
	}
	if t.conversion.Clearing == "" {
		if t.conversion.Clearing, err = a.ensureClearing(ctx, t); err != nil {
			return Reserved{}, err
		}
	}
	r, err := a.Ledger.ReserveConversion(ctx, t.conversion)
	if err != nil {
		return Reserved{}, ledgerError(err)
	}
	id := t.conversion.PaymentID
	if r.Refused {
		reason := "LIQUIDITY_UNAVAILABLE"
		if r.RefusedAccount == t.conversion.CustomerFunds {
			reason = "INSUFFICIENT_FUNDS"
		}
		return Reserved{Refused: true, OnProvider: reason == "LIQUIDITY_UNAVAILABLE"}, a.end(ctx, id, "reject", []PaymentStatus{Admitted}, Rejected, reason, Planned, Skipped)
	}
	return Reserved{ReservationID: r.ID}, a.Store.InTx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			insert into fx.trade (payment_id, reference, sell_currency, buy_currency, sell_amount, quoted_buy, provider_quote_id, reservation_id)
			select p.id, p.id::text, q.sell_currency, q.buy_currency, q.net_sell, q.provider_buy, q.provider_quote_id, $2
			from fx.payment p join fx.quote q on q.id = p.quote_id where p.id = $1
			on conflict (payment_id) do nothing`,
			id, r.ID,
		)
		if err != nil {
			return err
		}
		return a.paymentStep(ctx, tx, id, paymentTransition("converting", []PaymentStatus{Admitted}, Converting, ""))
	})
}

func (a *Activities) ensureClearing(ctx context.Context, t terms) (string, error) {
	id, err := a.Ledger.EnsureClearing(ctx, t.conversion.PaymentID, t.conversion.BuyCurrency)
	if err != nil {
		return "", err
	}
	_, err = a.Store.Pool().Exec(ctx, "update fx.payout set clearing_account_id = $2 where payment_id = $1 and clearing_account_id is null", t.conversion.PaymentID, id)
	return id, err
}

// ExpireConversion ends a payment that is too late to trade. A resumed run re-issues any reserve
// it already recorded with the same command, so a reservation it finds is released first.
func (a *Activities) ExpireConversion(ctx context.Context, paymentID string) error {
	t, err := a.terms(ctx, paymentID)
	if err != nil {
		return err
	}
	var recorded bool
	reserve := ledger.ReserveConversionReference(t.conversion.PaymentID)
	err = a.Store.Pool().QueryRow(ctx, "select exists (select from fx.ledger_intent where namespace = $1 and reference = $2)",
		reserve.Namespace, reserve.Reference).Scan(&recorded)
	if err != nil {
		return err
	}
	if recorded {
		r, err := a.Ledger.ReserveConversion(ctx, t.conversion)
		if err != nil {
			return ledgerError(err)
		}
		if !r.Refused {
			if err := a.Ledger.ReleaseConversion(ctx, t.conversion.PaymentID, r.ID); err != nil {
				return ledgerError(err)
			}
		}
	}
	return a.end(ctx, t.conversion.PaymentID, "expire", []PaymentStatus{Admitted, Converting}, Expired, "RATE_UNAVAILABLE", Planned, Skipped)
}

func (a *Activities) ReleaseConversion(ctx context.Context, paymentID string) error {
	t, err := a.terms(ctx, paymentID)
	if err != nil {
		return err
	}
	if err := a.Ledger.ReleaseConversion(ctx, t.conversion.PaymentID, t.reservationID); err != nil {
		return ledgerError(err)
	}
	return a.end(ctx, t.conversion.PaymentID, "expire", []PaymentStatus{Converting}, Expired, "RATE_UNAVAILABLE", Planned, Skipped)
}

// CommitConversion books the fill and any execution difference in one entry, then marks the
// payment CONVERTED; the bought amount waits in the payment's clearing account.
func (a *Activities) CommitConversion(ctx context.Context, paymentID string, delivered int64) error {
	t, err := a.terms(ctx, paymentID)
	if err != nil {
		return err
	}
	entry, err := a.Ledger.CommitConversion(ctx, t.conversion, t.reservationID, delivered)
	if err != nil {
		return ledgerError(err)
	}
	return a.Store.InTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "update fx.trade set entry_id = $2 where payment_id = $1", t.conversion.PaymentID, entry); err != nil {
			return err
		}
		return a.paymentStep(ctx, tx, t.conversion.PaymentID, paymentTransition("converted", []PaymentStatus{Converting}, Converted, ""))
	})
}

// SubmitTrade marks the submission before the first post. The provider's create is idempotent by
// reference, so a retry posts the same body and receives the trade as it stands.
func (a *Activities) SubmitTrade(ctx context.Context, paymentID string) (TradeOutcome, error) {
	req, err := a.claimTrade(ctx, paymentID)
	if err != nil {
		return TradeOutcome{}, err
	}
	trade, err := a.Provider.CreateTrade(ctx, req)
	if err != nil {
		return TradeOutcome{}, providerError(err)
	}
	return a.tradeOutcome(ctx, paymentID, req, trade, "READ")
}

// ResolveTrade reads the reference and, if it is still not terminal, closes it at the provider,
// until the provider answers with a terminal outcome.
func (a *Activities) ResolveTrade(ctx context.Context, paymentID string) (TradeOutcome, error) {
	req, err := a.claimTrade(ctx, paymentID)
	if err != nil {
		return TradeOutcome{}, err
	}
	trade, err := a.Provider.Trade(ctx, paymentID)
	if err == nil {
		return a.tradeOutcome(ctx, paymentID, req, trade, "READ")
	}
	if !errors.Is(err, provider.ErrNotVisible) {
		return TradeOutcome{}, providerError(err)
	}
	fence, err := a.Provider.CloseTrade(ctx, paymentID)
	if err != nil {
		return TradeOutcome{}, providerError(err)
	}
	if fence.Status == provider.FenceClosed {
		return a.tradeOutcome(ctx, paymentID, req, provider.Trade{Reference: paymentID, Status: provider.TradeClosed}, "FENCE")
	}
	if trade, err = a.Provider.Trade(ctx, paymentID); err != nil {
		return TradeOutcome{}, providerError(err)
	}
	return a.tradeOutcome(ctx, paymentID, req, trade, "READ")
}

func (a *Activities) claimTrade(ctx context.Context, paymentID string) (provider.TradeRequest, error) {
	var req provider.TradeRequest
	var sell int64
	err := a.Store.Pool().QueryRow(ctx, `
		update fx.trade set submission_started_at = coalesce(submission_started_at, now())
		where payment_id = $1
		returning reference, provider_quote_id, sell_currency, buy_currency, sell_amount`,
		paymentID,
	).Scan(&req.Reference, &req.QuoteID, &req.SellCurrency, &req.BuyCurrency, &sell)
	req.SellAmount = strconv.FormatInt(sell, 10)
	return req, err
}

func (a *Activities) tradeOutcome(ctx context.Context, paymentID string, req provider.TradeRequest, trade provider.Trade, source string) (TradeOutcome, error) {
	if trade.Status != provider.TradeFilled && trade.Status != provider.TradeRejected && trade.Status != provider.TradeClosed {
		return TradeOutcome{}, fmt.Errorf("trade %s is %s, not terminal", paymentID, trade.Status)
	}
	out := TradeOutcome{Status: trade.Status, Matches: true}
	var rate int64
	if trade.Status == provider.TradeFilled {
		var err error
		if out.Delivered, err = providerAmount(trade.BuyAmount); err != nil {
			return TradeOutcome{}, err
		}
		if rate, err = providerAmount(trade.RateE8); err != nil {
			return TradeOutcome{}, err
		}
		out.Matches = trade.Reference == req.Reference && trade.SellCurrency == req.SellCurrency &&
			trade.BuyCurrency == req.BuyCurrency && trade.SellAmount == req.SellAmount && out.Delivered > 0
	}
	return out, pgx.BeginFunc(ctx, a.Store.Pool(), func(tx pgx.Tx) error {
		id, _, err := store.RecordEvidence(ctx, tx, store.Evidence{Source: source, Operation: "TRADE", Reference: req.Reference, Outcome: trade.Status,
			Currency: trade.BuyCurrency, Amount: out.Delivered, Payload: trade})
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `
			update fx.trade set status = $2, provider_trade_id = nullif($3, ''), delivered_amount = nullif($4::bigint, 0), rate_e8 = nullif($5::bigint, 0), evidence_id = $6
			where payment_id = $1 and status = 'PENDING'`,
			paymentID, trade.Status, trade.TradeID, out.Delivered, rate, id,
		)
		return err
	})
}

func (a *Activities) StartPayout(ctx context.Context, paymentID string) error {
	id, err := parseID(paymentID)
	if err != nil {
		return err
	}
	return a.Store.TransitionPayout(ctx, id, payoutTransition("start-payout", []PayoutStatus{Planned}, Reserving, "PAYOUT_RESERVING"),
		func(ctx context.Context, tx pgx.Tx) error {
			return a.paymentStep(ctx, tx, id, paymentTransition("paying-out", []PaymentStatus{Converted}, PayingOut, ""))
		})
}

// ReservePayout retries the same attempt's command until the ledger answers, so a lost reply
// resolves to its stored outcome rather than to a new attempt.
func (a *Activities) ReservePayout(ctx context.Context, paymentID string, attempt int) (Reserved, error) {
	if err := a.Points.Hold(ctx, failpoint.PayoutBeforeReserve, paymentID); err != nil {
		return Reserved{}, err
	}
	t, err := a.terms(ctx, paymentID)
	if err != nil {
		return Reserved{}, err
	}
	r, err := a.Ledger.ReservePayout(ctx, t.payout, attempt)
	if err != nil {
		return Reserved{}, ledgerError(err)
	}
	if r.Refused {
		return Reserved{Refused: true, OnProvider: r.RefusedAccount == a.Ledger.ProviderCash(t.payout.Currency)}, nil
	}
	_, err = a.Store.Pool().Exec(ctx, "update fx.payout set reservation_id = $2 where payment_id = $1", paymentID, r.ID)
	return Reserved{ReservationID: r.ID}, err
}

func (a *Activities) WaitFunds(ctx context.Context, paymentID string, attempt int) error {
	id, err := parseID(paymentID)
	if err != nil {
		return err
	}
	return a.Store.TransitionPayout(ctx, id,
		payoutTransition(fmt.Sprintf("wait-funds/%d", attempt), []PayoutStatus{Reserving}, WaitingFunds, "LIQUIDITY_WAIT"), nil)
}

func (a *Activities) NextAttempt(ctx context.Context, paymentID string, attempt int) error {
	id, err := parseID(paymentID)
	if err != nil {
		return err
	}
	return a.Store.TransitionPayout(ctx, id,
		payoutTransition(fmt.Sprintf("next-attempt/%d", attempt), []PayoutStatus{WaitingFunds}, Reserving, "PAYOUT_RESERVING"),
		func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, "update fx.payout set attempt = $2 + 1 where payment_id = $1 and attempt = $2", paymentID, attempt)
			return err
		})
}

// SetReleaseTime opens the cancel window once, on the database clock.
func (a *Activities) SetReleaseTime(ctx context.Context, paymentID string) (time.Time, error) {
	t, err := a.terms(ctx, paymentID)
	if err != nil {
		return time.Time{}, err
	}
	var releaseAt time.Time
	err = a.Store.TransitionPayout(ctx, t.payout.PaymentID, payoutTransition("waiting-release", []PayoutStatus{Reserving}, WaitingRelease, string(WaitingRelease)),
		func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `
				update fx.payout set release_at = coalesce(release_at, now() + $2::interval) where payment_id = $1 returning release_at`,
				paymentID, t.policy.CancelGrace.String(),
			).Scan(&releaseAt)
		})
	return releaseAt, err
}

// PrepareSubmit closes the cancel window under the same row locks as a cancel decision, on the
// database clock read once the locks are held, so a cancel and a submission can never both win.
func (a *Activities) PrepareSubmit(ctx context.Context, paymentID string) (PrepareResult, error) {
	id, err := parseID(paymentID)
	if err != nil {
		return PrepareResult{}, err
	}
	var result PrepareResult
	err = pgx.BeginFunc(ctx, a.Store.Pool(), func(tx pgx.Tx) error {
		var (
			status, cancelCommand string
			due                   bool
		)
		err := tx.QueryRow(ctx, `
			select o.status, coalesce(o.cancel_command_id::text, '')
			from fx.payment p join fx.payout o on o.payment_id = p.id
			where p.id = $1
			for update of p, o`,
			paymentID,
		).Scan(&status, &cancelCommand)
		if err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, "select clock_timestamp() >= release_at from fx.payout where payment_id = $1", paymentID).Scan(&due); err != nil {
			return err
		}
		switch {
		case cancelCommand != "":
			result = PrepareResult{Result: SubmitCancelled, CancelCommandID: cancelCommand}
			return nil
		case PayoutStatus(status) == Submitting:
			result.Result = SubmitMoved
			return nil
		case !due:
			result.Result = SubmitEarly
			return nil
		}
		result.Result = SubmitMoved
		if _, err := tx.Exec(ctx, "update fx.payout set submission_started_at = coalesce(submission_started_at, now()) where payment_id = $1", paymentID); err != nil {
			return err
		}
		return transitionIn(ctx, tx, store.PayoutTable, id, payoutTransition("submitting", []PayoutStatus{WaitingRelease}, Submitting, string(Submitting)))
	})
	return result, err
}

func (a *Activities) SubmitPayout(ctx context.Context, paymentID string) (PayoutOutcome, error) {
	t, err := a.terms(ctx, paymentID)
	if err != nil {
		return PayoutOutcome{}, err
	}
	var (
		b           store.Beneficiary
		beneficiary string
		remittance  string
	)
	err = a.Store.Pool().QueryRow(ctx, `
		select b.id::text, b.name, b.account, b.bank_code, b.country, coalesce(p.remittance_info, '')
		from fx.payment p join fx.beneficiary b on b.id = p.beneficiary_id where p.id = $1`,
		paymentID,
	).Scan(&beneficiary, &b.Name, &b.Account, &b.BankCode, &b.Country, &remittance)
	if err != nil {
		return PayoutOutcome{}, err
	}
	payout, err := a.Provider.CreatePayout(ctx, provider.PayoutRequest{
		Reference: t.payout.PayoutID.String(), Currency: t.payout.Currency, Amount: strconv.FormatInt(t.payout.Amount, 10),
		Beneficiary:    provider.Beneficiary{ID: beneficiary, Name: b.Name, Account: b.Account, BankCode: b.BankCode, Country: b.Country},
		RemittanceInfo: remittance,
	})
	if err != nil {
		return PayoutOutcome{}, providerError(err)
	}
	out, err := a.payoutOutcome(ctx, t, payout, "READ")
	if err != nil || finalPayout(out.Status) {
		return out, err
	}
	return out, a.Store.TransitionPayout(ctx, t.payout.PaymentID, payoutTransition("sent", []PayoutStatus{Submitting}, Sent, string(Sent)),
		func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, "update fx.payout set provider_payout_id = $2 where payment_id = $1", paymentID, payout.PayoutID)
			return err
		})
}

// PollPayout reads the payout and fails with a retryable error while it is not terminal, so the
// activity's retries are the polling loop.
func (a *Activities) PollPayout(ctx context.Context, paymentID string) (PayoutOutcome, error) {
	t, err := a.terms(ctx, paymentID)
	if err != nil {
		return PayoutOutcome{}, err
	}
	payout, err := a.Provider.Payout(ctx, t.payout.PayoutID.String())
	if errors.Is(err, provider.ErrNotVisible) {
		return PayoutOutcome{}, temporal.NewApplicationError("the payout is not visible yet", ErrorPayoutProcessing)
	}
	if err != nil {
		return PayoutOutcome{}, providerError(err)
	}
	out, err := a.payoutOutcome(ctx, t, payout, "READ")
	if err == nil && out.Matches && !finalPayout(out.Status) {
		return out, temporal.NewApplicationError("the payout is "+payout.Status, ErrorPayoutProcessing)
	}
	return out, err
}

// VerifyPayout confirms a hint by reading the payout back. A hint is never evidence on its own.
func (a *Activities) VerifyPayout(ctx context.Context, paymentID string, h Hint) (PayoutOutcome, error) {
	t, err := a.terms(ctx, paymentID)
	if err != nil {
		return PayoutOutcome{}, err
	}
	if h.Reference != t.payout.PayoutID.String() {
		return PayoutOutcome{Status: h.Status}, nil
	}
	payout, err := a.Provider.Payout(ctx, h.Reference)
	if errors.Is(err, provider.ErrNotVisible) {
		return PayoutOutcome{Status: string(Sent), Matches: true}, nil
	}
	if err != nil {
		return PayoutOutcome{}, providerError(err)
	}
	return a.payoutOutcome(ctx, t, payout, "READ")
}

func (a *Activities) payoutOutcome(ctx context.Context, t terms, payout provider.Payout, source string) (PayoutOutcome, error) {
	out := PayoutOutcome{Status: payout.Status, FailureCode: payout.FailureCode, Matches: payoutMatches(t.payout, payout)}
	if !finalPayout(payout.Status) && out.Matches {
		return out, nil
	}
	amount, err := providerAmount(payout.Amount)
	if err != nil {
		return PayoutOutcome{}, err
	}
	id, _, err := store.RecordEvidence(ctx, a.Store.Pool(), store.Evidence{Source: source, Operation: "PAYOUT", Reference: payout.Reference, Outcome: payout.Status,
		Currency: payout.Currency, Amount: amount, Sequence: payout.Sequence, Payload: payout})
	out.EvidenceID = id
	return out, err
}

// payoutMatches decides whether the provider's answer is about this payout. A COMPLETED answer
// moves money, so it must carry the reference, the currency and the exact amount. Any other
// status may leave the terms out, as a fence on a reference the provider never saw does, but a
// term it does carry must still be ours.
func payoutMatches(want ledger.Payout, got provider.Payout) bool {
	if got.Reference != want.PayoutID.String() {
		return false
	}
	amount := strconv.FormatInt(want.Amount, 10)
	if got.Status == provider.PayoutCompleted {
		return got.Currency == want.Currency && got.Amount == amount
	}
	return (got.Currency == "" || got.Currency == want.Currency) && (got.Amount == "" || got.Amount == amount)
}

func (a *Activities) CommitPayout(ctx context.Context, paymentID string, ev PayoutOutcome) error {
	t, err := a.terms(ctx, paymentID)
	if err != nil {
		return err
	}
	entry, err := a.Ledger.CommitPayout(ctx, t.payout, t.payment.Payout.ReservationID)
	if err != nil {
		return ledgerError(err)
	}
	return a.Store.TransitionPayout(ctx, t.payout.PaymentID, payoutTransition("completed", []PayoutStatus{Submitting, Sent}, PayoutCompleted, "PAYOUT_COMPLETED"),
		func(ctx context.Context, tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, "update fx.payout set evidence_id = $2, entry_id = $3 where payment_id = $1", paymentID, ev.EvidenceID, entry); err != nil {
				return err
			}
			return a.paymentStep(ctx, tx, t.payout.PaymentID, paymentTransition("completed", []PaymentStatus{PayingOut}, Completed, ""))
		})
}

// ReturnPayout releases the payout reservation with the return journal after final no-payment
// evidence, and ends the payment as PAYOUT_FAILED.
func (a *Activities) ReturnPayout(ctx context.Context, paymentID string, ev PayoutOutcome) error {
	t, err := a.terms(ctx, paymentID)
	if err != nil {
		return err
	}
	entry, err := a.Ledger.ReleasePayout(ctx, t.payout, t.payment.Payout.ReservationID)
	if err != nil {
		return ledgerError(err)
	}
	reason := "PAYOUT_REJECTED"
	if ev.FailureCode == failureAccountClosed {
		reason = "BENEFICIARY_ACCOUNT_CLOSED"
	}
	return a.Store.TransitionPayout(ctx, t.payout.PaymentID, payoutTransition("failed", []PayoutStatus{Submitting, Sent}, Failed, "PAYOUT_FAILED"),
		func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, "update fx.payout set evidence_id = $2, entry_id = $3, returned_amount = amount where payment_id = $1", paymentID, ev.EvidenceID, entry)
			if err != nil {
				return err
			}
			return a.paymentStep(ctx, tx, t.payout.PaymentID, paymentTransition("payout-failed", []PaymentStatus{PayingOut}, PayoutFailed, reason))
		})
}

// CancelPayout applies an accepted cancel: a reserved payout is released with the return journal,
// an unreserved one is returned directly, and exactly one of the two ever happens, decided by the
// stored outcome of the reserve command. A retry after the last transaction committed finds its
// own command applied and succeeds; a payout left in CANCELLING by a crash continues from there.
func (a *Activities) CancelPayout(ctx context.Context, paymentID, commandID string) error {
	command, err := parseID(commandID)
	if err != nil {
		return err
	}
	t, err := a.terms(ctx, paymentID)
	if err != nil {
		return err
	}
	id := t.payout.PaymentID
	var cancelled bool
	err = a.Store.Pool().QueryRow(ctx, `
		select exists (
			select from fx.payout o join fx.product_command c on c.id = o.cancel_command_id
			where o.payment_id = $1 and o.status = 'CANCELLED' and o.cancel_command_id = $2 and c.state = 'APPLIED'
		)`,
		id, command,
	).Scan(&cancelled)
	if err != nil || cancelled {
		return err
	}
	err = a.Store.TransitionPayout(ctx, id, payoutTransition("cancelling", []PayoutStatus{Planned, Reserving, WaitingFunds, WaitingRelease}, Cancelling, "PAYOUT_CANCELLING"), nil)
	if err != nil {
		return transitionError(err)
	}
	var entry string
	if t.payment.Payout.ReservationID != "" {
		entry, err = a.Ledger.ReleasePayout(ctx, t.payout, t.payment.Payout.ReservationID)
	} else {
		entry, err = a.Ledger.ReturnUnreservedPayout(ctx, t.payout)
	}
	if err != nil {
		return ledgerError(err)
	}
	return a.Store.TransitionPayout(ctx, id, payoutTransition("cancelled", []PayoutStatus{Cancelling}, Cancelled, "PAYOUT_CANCELLED"),
		func(ctx context.Context, tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, "update fx.payout set entry_id = $2, returned_amount = amount where payment_id = $1", paymentID, entry); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `
				update fx.product_command set state = 'APPLIED', effect_ids = array[$2], applied_at = now(), delivery_pending = false
				where id = $1 and state = 'ACCEPTED'`, commandID, entry); err != nil {
				return err
			}
			return a.paymentStep(ctx, tx, id, paymentTransition("payout-cancelled", []PaymentStatus{Converted, PayingOut}, PayoutCancelled, "CUSTOMER_CANCELLED"))
		})
}

func (a *Activities) ApplyCommand(ctx context.Context, commandID string) error {
	_, err := a.Store.Pool().Exec(ctx, `
		update fx.product_command set state = 'APPLIED', applied_at = now(), delivery_pending = false
		where id = $1 and state = 'ACCEPTED'`, commandID)
	return err
}

func (a *Activities) OpenReview(ctx context.Context, paymentID string, reason ReviewReason) error {
	id, err := parseID(paymentID)
	if err != nil {
		return err
	}
	return pgx.BeginFunc(ctx, a.Store.Pool(), func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			update fx.payment set review_open = true, review_reason = $2, review_revision = review_revision + 1,
			                      review_opened_at = now(), version = version + 1, updated_at = now()
			where id = $1 and not review_open`,
			paymentID, reason,
		)
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}
		return store.AddEvent(ctx, tx, id, "REVIEW_OPENED", string(reason), "CUSTOMER", "review/"+string(reason))
	})
}

func (a *Activities) MarkRecoveryRequired(ctx context.Context, paymentID string) error {
	return a.OpenReview(ctx, paymentID, ReviewRecoveryRequired)
}

// end writes a payment's final status and its payout's, in one transaction, after the ledger call
// it depends on has answered.
func (a *Activities) end(ctx context.Context, payment uuid.UUID, key string, from []PaymentStatus, to PaymentStatus, reason string, payoutFrom, payoutTo PayoutStatus) error {
	return a.Store.TransitionPayout(ctx, payment, payoutTransition(key, []PayoutStatus{payoutFrom}, payoutTo, "PAYOUT_"+string(payoutTo)),
		func(ctx context.Context, tx pgx.Tx) error {
			return a.paymentStep(ctx, tx, payment, paymentTransition(key, from, to, reason))
		})
}

func (a *Activities) paymentStep(ctx context.Context, tx pgx.Tx, payment uuid.UUID, t store.Transition) error {
	return transitionIn(ctx, tx, store.PaymentTable, payment, t)
}

func transitionIn(ctx context.Context, tx pgx.Tx, table store.Table, payment uuid.UUID, t store.Transition) error {
	return transitionError(store.TransitionIn(ctx, tx, table, payment, t))
}

// paymentTransition names the event after the status it reaches, as the timeline shows it.
func paymentTransition(key string, from []PaymentStatus, to PaymentStatus, outcome string) store.Transition {
	return store.Transition{Key: key, From: statuses(from), To: string(to), Outcome: outcome, Event: string(to)}
}

func payoutTransition(key string, from []PayoutStatus, to PayoutStatus, event string) store.Transition {
	return store.Transition{Key: key, From: statuses(from), To: string(to), Event: event}
}

func statuses[S ~string](from []S) []string {
	names := make([]string, len(from))
	for i, status := range from {
		names[i] = string(status)
	}
	return names
}
