package ledger

import (
	"context"
	"errors"
	"fmt"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	ledgerv1 "github.com/xJohnNascimento/ledger/api/ledger/v1"
	"github.com/xJohnNascimento/ledger/api/ledger/v1/ledgerv1connect"

	"github.com/xJohnNascimento/corridor/internal/failpoint"
	"github.com/xJohnNascimento/corridor/internal/money"
)

// ErrUnexpected marks a ledger answer the payment flow does not cover, such as a reservation
// finalized by another command: money stays where it is and the payment goes to review.
var ErrUnexpected = errors.New("unexpected ledger state")

type Accounts struct {
	LedgerID      string
	ProviderCash  map[string]string
	FeeRevenue    map[string]string
	SpreadRevenue map[string]string
	FXDifference  map[string]string
}

// ResolveAccounts finds or creates the house and provider accounts by natural key, so ids are
// never configured by hand.
func ResolveAccounts(ctx context.Context, svc ledgerv1connect.LedgerServiceClient, ledgerID, providerOwner, houseOwner string, currencies []string) (Accounts, error) {
	a := Accounts{
		LedgerID: ledgerID, ProviderCash: map[string]string{}, FeeRevenue: map[string]string{},
		SpreadRevenue: map[string]string{}, FXDifference: map[string]string{},
	}
	for _, currency := range currencies {
		for _, target := range []struct {
			into    map[string]string
			owner   Owner
			purpose string
		}{
			{a.ProviderCash, Provider(providerOwner), "cash"},
			{a.FeeRevenue, House(houseOwner), "fee_revenue"},
			{a.SpreadRevenue, House(houseOwner), "spread_revenue"},
			{a.FXDifference, House(houseOwner), "fx_difference"},
		} {
			id, err := EnsureAccount(ctx, svc, ledgerID, target.owner, target.purpose, currency)
			if err != nil {
				return Accounts{}, fmt.Errorf("ensure %s %s: %w", target.purpose, currency, err)
			}
			target.into[currency] = id
		}
	}
	return a, nil
}

// Intents stores what Corridor is about to ask the ledger and what it answered.
type Intents interface {
	RecordIntent(ctx context.Context, namespace, reference, operation string, payment uuid.UUID, request any) error
	ObserveIntent(ctx context.Context, namespace, reference string, observed any) error
}

type Ledger struct {
	svc      ledgerv1connect.LedgerServiceClient
	intents  Intents
	points   *failpoint.Points
	accounts Accounts
}

func New(svc ledgerv1connect.LedgerServiceClient, intents Intents, points *failpoint.Points, accounts Accounts) *Ledger {
	return &Ledger{svc: svc, intents: intents, points: points, accounts: accounts}
}

// ProviderCash is the ledger account that holds Corridor's cash at the provider in a currency.
func (l *Ledger) ProviderCash(currency string) string {
	return l.accounts.ProviderCash[currency]
}

// EnsureClearing finds or creates the payment's clearing account in the bought currency.
func (l *Ledger) EnsureClearing(ctx context.Context, payment uuid.UUID, currency string) (string, error) {
	return EnsureAccount(ctx, l.svc, l.accounts.LedgerID, Payment(payment.String()), "clearing", currency)
}

// ReserveConversionReference is the command reference of a payment's conversion reservation.
func ReserveConversionReference(payment uuid.UUID) *ledgerv1.Reference {
	return ref("corridor.reserve-conversion", payment.String()+"/1")
}

type Reservation struct {
	ID             string `json:"id,omitempty"`
	Refused        bool   `json:"refused,omitempty"`
	Reason         string `json:"reason,omitempty"`
	RefusedAccount string `json:"refused_account,omitempty"`
}

type Conversion struct {
	PaymentID     uuid.UUID
	CustomerFunds string
	Clearing      string
	SellCurrency  string
	BuyCurrency   string
	Sell          int64
	Fee           int64
	Net           int64
	ProviderBuy   int64
	Buy           int64
	Spread        int64
}

type Payout struct {
	PaymentID     uuid.UUID
	PayoutID      uuid.UUID
	Clearing      string
	CustomerFunds string
	Currency      string
	Amount        int64
}

func (l *Ledger) ReserveConversion(ctx context.Context, c Conversion) (Reservation, error) {
	sell, buy := c.SellCurrency, c.BuyCurrency
	return l.reserve(ctx, c.PaymentID, ReserveConversionReference(c.PaymentID), ref("corridor.conversion", c.PaymentID.String()),
		[]*ledgerv1.ReservationLeg{
			{AccountId: c.CustomerFunds, MaxReduction: money.Format(c.Sell)},
			{AccountId: l.accounts.ProviderCash[sell], MaxReduction: money.Format(c.Net)},
		},
		[]string{l.accounts.FeeRevenue[sell], l.accounts.ProviderCash[buy], c.Clearing, l.accounts.SpreadRevenue[buy], l.accounts.FXDifference[buy]},
	)
}

// CommitConversion posts the conversion and any execution difference in one entry, so a fill and
// its difference become visible together.
func (l *Ledger) CommitConversion(ctx context.Context, c Conversion, reservationID string, delivered int64) (string, error) {
	sell, buy := c.SellCurrency, c.BuyCurrency
	j := journal{kind: "fx.conversion", product: "payment/" + c.PaymentID.String()}
	j.debit(c.CustomerFunds, sell, c.Sell)
	j.credit(l.accounts.FeeRevenue[sell], sell, c.Fee)
	j.credit(l.accounts.ProviderCash[sell], sell, c.Net)
	j.debit(l.accounts.ProviderCash[buy], buy, c.ProviderBuy)
	j.credit(c.Clearing, buy, c.Buy)
	j.credit(l.accounts.SpreadRevenue[buy], buy, c.Spread)
	switch difference := delivered - c.ProviderBuy; {
	case difference < 0:
		j.debit(l.accounts.FXDifference[buy], buy, -difference)
		j.credit(l.accounts.ProviderCash[buy], buy, -difference)
	case difference > 0:
		j.debit(l.accounts.ProviderCash[buy], buy, difference)
		j.credit(l.accounts.FXDifference[buy], buy, difference)
	}
	return l.commit(ctx, c.PaymentID, ref("corridor.commit-conversion", c.PaymentID.String()), reservationID, j)
}

func (l *Ledger) ReleaseConversion(ctx context.Context, payment uuid.UUID, reservationID string) error {
	_, err := l.release(ctx, payment, ref("corridor.release-conversion", payment.String()), reservationID, nil)
	return err
}

func (l *Ledger) ReservePayout(ctx context.Context, p Payout, attempt int) (Reservation, error) {
	provider := l.accounts.ProviderCash[p.Currency]
	return l.reserve(ctx, p.PaymentID, ref("corridor.reserve-payout", fmt.Sprintf("%s/%d", p.PayoutID, attempt)), ref("corridor.payout", p.PayoutID.String()),
		[]*ledgerv1.ReservationLeg{
			{AccountId: p.Clearing, MaxReduction: money.Format(p.Amount)},
			{AccountId: provider, MaxReduction: money.Format(p.Amount)},
		},
		[]string{p.CustomerFunds},
	)
}

func (l *Ledger) CommitPayout(ctx context.Context, p Payout, reservationID string) (string, error) {
	j := journal{kind: "fx.payout", product: "payout/" + p.PayoutID.String()}
	j.debit(p.Clearing, p.Currency, p.Amount)
	j.credit(l.accounts.ProviderCash[p.Currency], p.Currency, p.Amount)
	return l.commit(ctx, p.PaymentID, ref("corridor.commit-payout", p.PayoutID.String()), reservationID, j)
}

// ReleasePayout releases the payout reservation and returns the bought amount to the customer in
// the same transaction.
func (l *Ledger) ReleasePayout(ctx context.Context, p Payout, reservationID string) (string, error) {
	j := returnJournal(p)
	return l.release(ctx, p.PaymentID, ref("corridor.release-payout", p.PayoutID.String()), reservationID, &j)
}

func (l *Ledger) ReturnUnreservedPayout(ctx context.Context, p Payout) (string, error) {
	command := ref("corridor.return-unreserved-payout", p.PayoutID.String())
	j := returnJournal(p)
	req := &ledgerv1.PostJournalRequest{Command: command, LedgerId: l.accounts.LedgerID, Journal: j.message()}
	if err := l.intents.RecordIntent(ctx, command.Namespace, command.Reference, "POST_JOURNAL", p.PaymentID, req); err != nil {
		return "", err
	}
	resp, err := l.svc.PostJournal(ctx, connect.NewRequest(req))
	if err != nil {
		return "", classify(err)
	}
	return resp.Msg.Entry.Id, l.answered(ctx, p.PaymentID, command, resp.Msg.Entry.Id)
}

func returnJournal(p Payout) journal {
	j := journal{kind: "fx.payout-return", product: "payout/" + p.PayoutID.String()}
	j.debit(p.Clearing, p.Currency, p.Amount)
	j.credit(p.CustomerFunds, p.Currency, p.Amount)
	return j
}

func (l *Ledger) reserve(ctx context.Context, payment uuid.UUID, command, logical *ledgerv1.Reference, legs []*ledgerv1.ReservationLeg, scope []string) (Reservation, error) {
	req := &ledgerv1.ReserveFundsRequest{Command: command, LedgerId: l.accounts.LedgerID, ReservationRef: logical, Legs: legs, ScopeAccountIds: scope}
	if err := l.intents.RecordIntent(ctx, command.Namespace, command.Reference, "RESERVE_FUNDS", payment, req); err != nil {
		return Reservation{}, err
	}
	resp, err := l.svc.ReserveFunds(ctx, connect.NewRequest(req))
	var r Reservation
	switch reason, detail := reasonOf(err); {
	case err == nil:
		r.ID = resp.Msg.Reservation.Id
	case reason == ledgerv1.Reason_REASON_INSUFFICIENT_FUNDS || reason == ledgerv1.Reason_REASON_ACCOUNT_BLOCKED:
		r = Reservation{Refused: true, Reason: reason.String(), RefusedAccount: detail.AccountId}
	default:
		return Reservation{}, classify(err)
	}
	return r, l.answered(ctx, payment, command, r)
}

func (l *Ledger) commit(ctx context.Context, payment uuid.UUID, command *ledgerv1.Reference, reservationID string, j journal) (string, error) {
	req := &ledgerv1.CommitReservationRequest{Command: command, ReservationId: reservationID, Journal: j.message()}
	if err := l.intents.RecordIntent(ctx, command.Namespace, command.Reference, "COMMIT_RESERVATION", payment, req); err != nil {
		return "", err
	}
	resp, err := l.svc.CommitReservation(ctx, connect.NewRequest(req))
	if err != nil {
		return "", classify(err)
	}
	return resp.Msg.Entry.Id, l.answered(ctx, payment, command, resp.Msg.Entry.Id)
}

func (l *Ledger) release(ctx context.Context, payment uuid.UUID, command *ledgerv1.Reference, reservationID string, ret *journal) (string, error) {
	req := &ledgerv1.ReleaseReservationRequest{Command: command, ReservationId: reservationID}
	if ret != nil {
		req.ReturnJournal = ret.message()
	}
	if err := l.intents.RecordIntent(ctx, command.Namespace, command.Reference, "RELEASE_RESERVATION", payment, req); err != nil {
		return "", err
	}
	resp, err := l.svc.ReleaseReservation(ctx, connect.NewRequest(req))
	if err != nil {
		return "", classify(err)
	}
	entry := resp.Msg.Entry.GetId()
	return entry, l.answered(ctx, payment, command, entry)
}

// answered records what the ledger said, then lets a scenario lose or hold that answer: the
// effect is committed, and the caller retries the same reference to learn it again.
func (l *Ledger) answered(ctx context.Context, payment uuid.UUID, command *ledgerv1.Reference, observed any) error {
	if err := l.intents.ObserveIntent(ctx, command.Namespace, command.Reference, observed); err != nil {
		return err
	}
	if l.points.Hit(ctx, failpoint.LedgerDropReply, payment.String()) {
		return fmt.Errorf("ledger reply for %s/%s: %w", command.Namespace, command.Reference, failpoint.ErrInjected)
	}
	return l.points.Hold(ctx, failpoint.LedgerHoldReply, payment.String())
}

func ref(namespace, reference string) *ledgerv1.Reference {
	return &ledgerv1.Reference{Namespace: namespace, Reference: reference}
}

type journal struct {
	kind    string
	product string
	lines   []*ledgerv1.JournalLine
}

func (j *journal) debit(account, asset string, amount int64) {
	j.add(account, ledgerv1.Side_SIDE_DEBIT, asset, amount)
}

func (j *journal) credit(account, asset string, amount int64) {
	j.add(account, ledgerv1.Side_SIDE_CREDIT, asset, amount)
}

func (j *journal) add(account string, side ledgerv1.Side, asset string, amount int64) {
	if amount == 0 {
		return
	}
	j.lines = append(j.lines, &ledgerv1.JournalLine{AccountId: account, Side: side, AssetCode: asset, Amount: money.Format(amount)})
}

func (j journal) message() *ledgerv1.Journal {
	return &ledgerv1.Journal{Kind: j.kind, ProductReference: j.product, Lines: j.lines}
}

func reasonOf(err error) (ledgerv1.Reason, *ledgerv1.ErrorDetail) {
	var connectErr *connect.Error
	if !errors.As(err, &connectErr) {
		return ledgerv1.Reason_REASON_UNSPECIFIED, &ledgerv1.ErrorDetail{}
	}
	for _, d := range connectErr.Details() {
		if v, valueErr := d.Value(); valueErr == nil {
			if detail, ok := v.(*ledgerv1.ErrorDetail); ok {
				return detail.Reason, detail
			}
		}
	}
	return ledgerv1.Reason_REASON_UNSPECIFIED, &ledgerv1.ErrorDetail{}
}

// classify keeps infrastructure errors retryable and turns any refusal the flow did not expect,
// such as a reservation finalized by another command, into ErrUnexpected.
func classify(err error) error {
	if reason, _ := reasonOf(err); reason != ledgerv1.Reason_REASON_UNSPECIFIED {
		return fmt.Errorf("%w: %w", ErrUnexpected, err)
	}
	return err
}
