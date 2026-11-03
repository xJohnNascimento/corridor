package payment_test

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"

	"github.com/xJohnNascimento/corridor/internal/config"
	"github.com/xJohnNascimento/corridor/internal/payment"
	"github.com/xJohnNascimento/corridor/internal/store"
)

var acts *payment.Activities

var paymentStatusKey = temporal.NewSearchAttributeKeyKeyword("PaymentStatus")

type flow struct {
	*testsuite.TestWorkflowEnvironment
	t      *testing.T
	in     payment.Input
	ran    map[string]int
	status payment.PaymentStatus
}

// stub is an activity's answer: a value, an error, or a function with the activity's signature.
type stub struct {
	value any
	err   error
}

func newFlow(t *testing.T, status payment.PaymentStatus, overrides map[string]stub) *flow {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflowWithOptions(payment.PaymentWorkflow, workflow.RegisterOptions{Name: "PaymentWorkflow"})
	env.RegisterActivity(acts)
	f := &flow{TestWorkflowEnvironment: env, t: t, ran: map[string]int{}}
	f.OnUpsertTypedSearchAttributes(mock.Anything).Return().Run(func(args mock.Arguments) {
		if value, ok := args.Get(0).(temporal.SearchAttributes).GetKeyword(paymentStatusKey); ok {
			f.status = payment.PaymentStatus(value)
		}
	})
	start := env.Now()
	f.in = payment.Input{
		PaymentID: "p-1", CustomerID: "acme", PayoutID: "o-1",
		Quote: payment.QuoteSnapshot{SellCurrency: "CAD", BuyCurrency: "USD", Sell: 1000000, Fee: 5000, Net: 995000,
			ProviderBuy: 731325, Buy: 730959, Spread: 366, ProviderQuoteID: "pq-1", ProviderExpiresAt: start.Add(60 * time.Second)},
		Policy: config.Policy{CancelGrace: 20 * time.Second, FastPathWait: 2 * time.Second, TradeSafety: 5 * time.Second,
			PollInitial: 2 * time.Second, PollMax: 10 * time.Second, PollHorizon: 2 * time.Minute, LiquidityRetry: 10 * time.Second, FillToleranceBps: 10},
	}
	defaults := []struct {
		activity any
		stub     stub
	}{
		{acts.Load, stub{value: payment.Snapshot{PaymentStatus: status, PayoutStatus: payment.Planned, Attempt: 1}}},
		{acts.ReserveConversion, stub{value: payment.Reserved{ReservationID: "r-1"}}},
		{acts.SubmitTrade, stub{value: payment.TradeOutcome{Status: "FILLED", Delivered: 731325, Matches: true}}},
		{acts.ResolveTrade, stub{value: payment.TradeOutcome{Status: "CLOSED", Matches: true}}},
		{acts.CommitConversion, stub{}},
		{acts.StartPayout, stub{}},
		{acts.ReservePayout, stub{value: payment.Reserved{ReservationID: "r-2"}}},
		{acts.SetReleaseTime, stub{value: start.Add(20 * time.Second)}},
		{acts.PrepareSubmit, stub{value: payment.PrepareResult{Result: payment.SubmitMoved}}},
		{acts.SubmitPayout, stub{value: payment.PayoutOutcome{Status: "ACCEPTED", Matches: true}}},
		{acts.PollPayout, stub{value: payment.PayoutOutcome{Status: "COMPLETED", Matches: true, EvidenceID: "e-1"}}},
		{acts.VerifyPayout, stub{value: payment.PayoutOutcome{Status: "SENT", Matches: true}}},
		{acts.DecideCommand, stub{value: payment.Decision{State: "REJECTED"}}},
		{acts.CommitPayout, stub{}}, {acts.ReleaseConversion, stub{}}, {acts.ExpireConversion, stub{}}, {acts.ReturnPayout, stub{}},
		{acts.CancelPayout, stub{}}, {acts.WaitFunds, stub{}}, {acts.NextAttempt, stub{}}, {acts.OpenReview, stub{}},
		{acts.ApplyCommand, stub{}}, {acts.MarkRecoveryRequired, stub{}},
		{acts.ApplyApproval, stub{value: payment.Applied{Done: true, PaymentStatus: payment.Converted, PayoutStatus: payment.Planned}}},
	}
	for _, d := range defaults {
		name := activityName(d.activity)
		answer := d.stub
		if o, ok := overrides[name]; ok {
			answer = o
		}
		var returns []any
		switch outputs := reflect.TypeOf(d.activity).NumOut(); {
		case reflect.TypeOf(answer.value) != nil && reflect.TypeOf(answer.value).Kind() == reflect.Func:
			returns = []any{answer.value}
		case outputs == 1:
			returns = []any{answer.err}
		case answer.value == nil:
			returns = []any{reflect.Zero(reflect.TypeOf(d.activity).Out(0)).Interface(), answer.err}
		default:
			returns = []any{answer.value, answer.err}
		}
		for _, args := range [][]any{{mock.Anything, mock.Anything}, {mock.Anything, mock.Anything, mock.Anything}} {
			f.OnActivity(d.activity, args...).Return(returns...).Maybe().Run(func(mock.Arguments) { f.ran[name]++ })
		}
	}
	return f
}

func (f *flow) run() {
	f.ExecuteWorkflow(payment.PaymentWorkflow, f.in)
}

func (f *flow) require(calls map[string]int) {
	f.t.Helper()
	for name, want := range calls {
		if f.ran[name] != want {
			f.t.Errorf("%s ran %d times, want %d (all: %v)", name, f.ran[name], want, f.ran)
		}
	}
}

// requireEnded asserts that the workflow closed cleanly with the payment in its final status.
func (f *flow) requireEnded(status payment.PaymentStatus) {
	f.t.Helper()
	if !f.IsWorkflowCompleted() || f.GetWorkflowError() != nil {
		f.t.Fatalf("completed %v, error %v", f.IsWorkflowCompleted(), f.GetWorkflowError())
	}
	if f.status != status {
		f.t.Errorf("the payment ended %s, want %s", f.status, status)
	}
}

// requireInReview asserts that the payment waits for an operator: the workflow only stops when
// the test clock runs out, with the payment still in the status it was reviewed in.
func (f *flow) requireInReview(status payment.PaymentStatus) {
	f.t.Helper()
	if err := f.GetWorkflowError(); !temporal.IsTimeoutError(err) {
		f.t.Errorf("the workflow ended with %v, want it waiting for a review", err)
	}
	if f.status != status {
		f.t.Errorf("the payment is %s in review, want %s", f.status, status)
	}
}

// approve delivers a command through the executeCommand Update, as the dispatcher does once the
// command was stored.
func (f *flow) approve(t *testing.T, commandID string, after time.Duration) {
	f.RegisterDelayedCallback(func() {
		f.UpdateWorkflow(payment.UpdateExecuteCommand, commandID, &testsuite.TestUpdateCallback{
			OnAccept: func() {}, OnReject: func(err error) { t.Errorf("rejected: %v", err) }, OnComplete: func(any, error) {},
		}, commandID)
	}, after)
}

func TestHappyPathRunsEveryStepOnce(t *testing.T) {
	f := newFlow(t, payment.Admitted, nil)
	f.run()
	f.requireEnded(payment.Completed)
	f.require(map[string]int{"ReserveConversion": 1, "SubmitTrade": 1, "CommitConversion": 1, "StartPayout": 1, "ReservePayout": 1,
		"SetReleaseTime": 1, "PrepareSubmit": 1, "SubmitPayout": 1, "CommitPayout": 1, "ReturnPayout": 0, "OpenReview": 0})
}

func TestRefusedReservationEndsBeforeAnyTrade(t *testing.T) {
	f := newFlow(t, payment.Admitted, map[string]stub{"ReserveConversion": {value: payment.Reserved{Refused: true}}})
	f.run()
	f.requireEnded(payment.Rejected)
	f.require(map[string]int{"SubmitTrade": 0, "CommitConversion": 0, "StartPayout": 0})
}

func TestRejectedTradeReleasesTheConversion(t *testing.T) {
	f := newFlow(t, payment.Admitted, map[string]stub{"SubmitTrade": {value: payment.TradeOutcome{Status: "REJECTED", Matches: true}}})
	f.run()
	f.requireEnded(payment.Expired)
	f.require(map[string]int{"ReleaseConversion": 1, "CommitConversion": 0, "StartPayout": 0})
}

func TestTooLateToTradeExpiresWithoutReserving(t *testing.T) {
	f := newFlow(t, payment.Admitted, nil)
	f.in.Quote.ProviderExpiresAt = f.Now().Add(3 * time.Second)
	f.run()
	f.requireEnded(payment.Expired)
	f.require(map[string]int{"ExpireConversion": 1, "ReserveConversion": 0, "SubmitTrade": 0})
}

func TestFillsAreBookedOrReviewedByTolerance(t *testing.T) {
	fill := func(delivered int64, matches bool) map[string]stub {
		return map[string]stub{"SubmitTrade": {value: payment.TradeOutcome{Status: "FILLED", Delivered: delivered, Matches: matches}}}
	}

	t.Run("a fill within tolerance is booked", func(t *testing.T) {
		f := newFlow(t, payment.Admitted, fill(731325-500, true))
		f.run()
		f.requireEnded(payment.Completed)
		f.require(map[string]int{"CommitConversion": 1, "OpenReview": 0})
	})

	t.Run("a fill beyond tolerance opens a review and moves nothing", func(t *testing.T) {
		f := newFlow(t, payment.Admitted, fill(731325-732, true))
		f.RegisterDelayedCallback(func() {}, time.Hour)
		f.run()
		f.require(map[string]int{"CommitConversion": 0, "OpenReview": 1, "StartPayout": 0})
		f.requireInReview(payment.Converting)
	})

	t.Run("a fill that does not match its trade is a breach and opens a review", func(t *testing.T) {
		f := newFlow(t, payment.Admitted, fill(731325, false))
		f.RegisterDelayedCallback(func() {}, time.Hour)
		f.run()
		f.require(map[string]int{"CommitConversion": 0, "OpenReview": 1})
	})
}

func TestAnApprovedReviewIsAppliedAndThePaymentGoesOn(t *testing.T) {
	approval := &payment.Approval{ProposalID: "pr-1", CommandID: "c-1", Decision: "ACCEPT_FILL", Delivered: 731325 - 732}
	f := newFlow(t, payment.Admitted, map[string]stub{
		"SubmitTrade":   {value: payment.TradeOutcome{Status: "FILLED", Delivered: 731325 - 732, Matches: true}},
		"DecideCommand": {value: payment.Decision{CommandID: "c-1", State: payment.Accepted, Approval: approval}},
		"ApplyApproval": {value: payment.Applied{Done: true, PaymentStatus: payment.Converted, PayoutStatus: payment.Planned}},
	})
	f.approve(t, "c-1", time.Hour)
	f.run()
	f.requireEnded(payment.Completed)
	f.require(map[string]int{"OpenReview": 1, "ApplyApproval": 1, "CommitConversion": 0, "StartPayout": 1, "CommitPayout": 1})
}

func TestAnApprovalTheProviderContradictsKeepsTheReviewOpen(t *testing.T) {
	applied := 0
	f := newFlow(t, payment.Admitted, map[string]stub{
		"SubmitTrade": {value: payment.TradeOutcome{Status: "FILLED", Delivered: 731325 - 732, Matches: true}},
		"DecideCommand": {value: func(_ context.Context, _, commandID string) (payment.Decision, error) {
			return payment.Decision{CommandID: commandID, State: payment.Accepted, Approval: &payment.Approval{ProposalID: "pr-" + commandID, CommandID: commandID, Decision: "ACCEPT_FILL"}}, nil
		}},
		"ApplyApproval": {value: func(context.Context, string, payment.Approval) (payment.Applied, error) {
			applied++
			if applied == 1 {
				return payment.Applied{}, nil
			}
			return payment.Applied{Done: true, PaymentStatus: payment.Converted, PayoutStatus: payment.Planned}, nil
		}},
	})
	f.approve(t, "c-1", time.Hour)
	f.approve(t, "c-2", 2*time.Hour)
	f.run()
	f.requireEnded(payment.Completed)
	f.require(map[string]int{"OpenReview": 1, "ApplyApproval": 2, "StartPayout": 1, "CommitPayout": 1})
}

func TestProviderCashShortWaitsAndTriesANewAttempt(t *testing.T) {
	attempts := 0
	f := newFlow(t, payment.Converted, map[string]stub{"ReservePayout": {value: func(_ context.Context, _ string, attempt int) (payment.Reserved, error) {
		attempts++
		if attempt == 1 {
			return payment.Reserved{Refused: true, OnProvider: true}, nil
		}
		return payment.Reserved{ReservationID: "r-2"}, nil
	}}})
	f.run()
	if attempts != 2 {
		t.Fatalf("%d reserve attempts, want 2", attempts)
	}
	f.requireEnded(payment.Completed)
	f.require(map[string]int{"WaitFunds": 1, "NextAttempt": 1, "CommitPayout": 1})
}

func TestACancelInTheReleaseWindowReturnsInsteadOfSubmitting(t *testing.T) {
	f := newFlow(t, payment.Converted, map[string]stub{"DecideCommand": {value: payment.Decision{State: payment.Accepted, Cancel: true}}})
	f.approve(t, "c-1", 5*time.Second)
	f.run()
	f.requireEnded(payment.PayoutCancelled)
	f.require(map[string]int{"CancelPayout": 1, "PrepareSubmit": 0, "SubmitPayout": 0, "CommitPayout": 0})
}

func TestARejectedPayoutIsReturned(t *testing.T) {
	f := newFlow(t, payment.Converted, map[string]stub{"SubmitPayout": {value: payment.PayoutOutcome{Status: "REJECTED", FailureCode: "AM04", Matches: true, EvidenceID: "e-1"}}})
	f.run()
	f.requireEnded(payment.PayoutFailed)
	f.require(map[string]int{"ReturnPayout": 1, "CommitPayout": 0, "PollPayout": 0})
}

func TestThePollHorizonOpensAReviewWithTheReservationIntact(t *testing.T) {
	f := newFlow(t, payment.Converted, map[string]stub{"PollPayout": {err: temporal.NewApplicationError("still processing", payment.ErrorPayoutProcessing)}})
	f.RegisterDelayedCallback(func() {}, 10*time.Minute)
	f.run()
	f.require(map[string]int{"OpenReview": 1, "CommitPayout": 0, "ReturnPayout": 0})
	f.requireInReview(payment.PayingOut)
}

func TestAContractBreachWhilePollingOpensTheBreachReviewNotTheHorizon(t *testing.T) {
	var reasons []payment.ReviewReason
	breach := errors.New("the provider answered another payout")
	f := newFlow(t, payment.Converted, map[string]stub{
		"PollPayout": {err: temporal.NewNonRetryableApplicationError(breach.Error(), payment.ErrorContractBreach, breach)},
		"OpenReview": {value: func(_ context.Context, _ string, reason payment.ReviewReason) error {
			reasons = append(reasons, reason)
			return nil
		}},
	})
	f.RegisterDelayedCallback(func() {}, time.Hour)
	f.run()
	if len(reasons) != 1 || reasons[0] != payment.ReviewProviderContractBreach {
		t.Fatalf("reviews opened for %v, want %s", reasons, payment.ReviewProviderContractBreach)
	}
	f.require(map[string]int{"CommitPayout": 0, "ReturnPayout": 0})
	f.requireInReview(payment.PayingOut)
}

func TestAHintMovesMoneyOnlyAfterItsReadBack(t *testing.T) {
	verified := 0
	f := newFlow(t, payment.Converted, map[string]stub{
		"PollPayout": {err: temporal.NewApplicationError("still processing", payment.ErrorPayoutProcessing)},
		"VerifyPayout": {value: func(_ context.Context, _ string, h payment.Hint) (payment.PayoutOutcome, error) {
			verified++
			if h.EventID == "evt-false" {
				return payment.PayoutOutcome{Status: "SENT", Matches: true}, nil
			}
			return payment.PayoutOutcome{Status: "COMPLETED", Matches: true, EvidenceID: "e-1"}, nil
		}},
	})
	for _, at := range []struct {
		event string
		after time.Duration
	}{{"evt-false", 30 * time.Second}, {"evt-false", 31 * time.Second}, {"evt-true", 40 * time.Second}} {
		f.RegisterDelayedCallback(func() {
			f.SignalWorkflow(payment.SignalProviderHint, payment.Hint{EventID: at.event, Reference: "o-1", Status: "FAILED"})
		}, at.after)
	}
	f.run()
	if verified != 2 {
		t.Fatalf("%d read-backs, want one per distinct event", verified)
	}
	f.requireEnded(payment.Completed)
	f.require(map[string]int{"CommitPayout": 1, "ReturnPayout": 0})
}

func TestAnUncoveredErrorFailsTheTaskNotThePayment(t *testing.T) {
	f := newFlow(t, payment.Admitted, map[string]stub{"StartPayout": {err: temporal.NewNonRetryableApplicationError("bug", payment.ErrorTransitionConflict, errors.New("bug"))}})
	f.run()
	if f.GetWorkflowError() == nil {
		t.Fatal("an uncovered transition must fail the workflow task")
	}
	f.require(map[string]int{"CommitPayout": 0, "ReturnPayout": 0})
}

func TestACancelWhileWaitingForProviderFundsStopsTheWait(t *testing.T) {
	f := newFlow(t, payment.Converted, map[string]stub{
		"ReservePayout": {value: payment.Reserved{Refused: true, OnProvider: true}},
		"DecideCommand": {value: payment.Decision{State: payment.Accepted, Cancel: true}},
	})
	f.approve(t, "c-1", 3*time.Second)
	f.run()
	f.requireEnded(payment.PayoutCancelled)
	f.require(map[string]int{"ReservePayout": 1, "WaitFunds": 1, "NextAttempt": 0, "CancelPayout": 1, "ApplyCommand": 1, "CommitPayout": 0})
}

func TestATradeSubmissionThatTimesOutIsResolvedByReference(t *testing.T) {
	timedOut := stub{err: temporal.NewTimeoutError(enumspb.TIMEOUT_TYPE_SCHEDULE_TO_CLOSE, nil)}
	cases := map[string]struct {
		resolved payment.TradeOutcome
		ends     payment.PaymentStatus
		calls    map[string]int
	}{
		"the provider filled it": {
			payment.TradeOutcome{Status: "FILLED", Delivered: 731325, Matches: true}, payment.Completed,
			map[string]int{"CommitConversion": 1, "ReleaseConversion": 0, "CommitPayout": 1},
		},
		"the provider closed it": {
			payment.TradeOutcome{Status: "CLOSED", Matches: true}, payment.Expired,
			map[string]int{"CommitConversion": 0, "ReleaseConversion": 1, "StartPayout": 0},
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFlow(t, payment.Admitted, map[string]stub{"SubmitTrade": timedOut, "ResolveTrade": {value: c.resolved}})
			f.run()
			f.requireEnded(c.ends)
			f.require(c.calls)
			f.require(map[string]int{"SubmitTrade": 1, "ResolveTrade": 1})
		})
	}
}

func TestAHintThatDoesNotMatchTheProviderOpensAReviewInsteadOfMovingMoney(t *testing.T) {
	var reasons []payment.ReviewReason
	f := newFlow(t, payment.Converted, map[string]stub{
		"PollPayout": {err: temporal.NewApplicationError("still processing", payment.ErrorPayoutProcessing)},
		"VerifyPayout": {value: func(context.Context, string, payment.Hint) (payment.PayoutOutcome, error) {
			return payment.PayoutOutcome{Status: "COMPLETED", Matches: false, EvidenceID: "e-1"}, nil
		}},
		"OpenReview": {value: func(_ context.Context, _ string, reason payment.ReviewReason) error {
			reasons = append(reasons, reason)
			return nil
		}},
	})
	f.RegisterDelayedCallback(func() {
		f.SignalWorkflow(payment.SignalProviderHint, payment.Hint{EventID: "evt-1", Reference: "o-1", Status: "COMPLETED"})
	}, 30*time.Second)
	f.RegisterDelayedCallback(func() {}, time.Hour)
	f.run()
	if len(reasons) != 1 || reasons[0] != payment.ReviewConflictingProviderEvidence {
		t.Fatalf("reviews opened for %v, want CONFLICTING_PROVIDER_EVIDENCE", reasons)
	}
	f.require(map[string]int{"VerifyPayout": 1, "CommitPayout": 0, "ReturnPayout": 0})
	f.requireInReview(payment.PayingOut)
}

func TestARejectedAnswerToAHintIsFinalLikeAnyOther(t *testing.T) {
	f := newFlow(t, payment.Converted, map[string]stub{
		"PollPayout":   {err: temporal.NewApplicationError("still processing", payment.ErrorPayoutProcessing)},
		"VerifyPayout": {value: payment.PayoutOutcome{Status: "REJECTED", FailureCode: "AM04", Matches: true, EvidenceID: "e-1"}},
	})
	f.RegisterDelayedCallback(func() {
		f.SignalWorkflow(payment.SignalProviderHint, payment.Hint{EventID: "evt-1", Reference: "o-1", Status: "REJECTED"})
	}, 30*time.Second)
	f.run()
	f.requireEnded(payment.PayoutFailed)
	f.require(map[string]int{"VerifyPayout": 1, "ReturnPayout": 1, "CommitPayout": 0})
}

func TestAHintWhoseReadBackKeepsFailingDoesNotHoldBackThePollHorizon(t *testing.T) {
	var (
		f        *flow
		start    time.Time
		reviewAt = -time.Second
		reasons  []payment.ReviewReason
	)
	f = newFlow(t, payment.Converted, map[string]stub{
		"PollPayout":   {err: temporal.NewApplicationError("still processing", payment.ErrorPayoutProcessing)},
		"VerifyPayout": {err: errors.New("the provider is unavailable")},
		"OpenReview": {value: func(_ context.Context, _ string, reason payment.ReviewReason) error {
			reasons = append(reasons, reason)
			reviewAt = f.Now().Sub(start)
			return nil
		}},
	})
	start = f.Now()
	f.RegisterDelayedCallback(func() {
		f.SignalWorkflow(payment.SignalProviderHint, payment.Hint{EventID: "evt-1", Reference: "o-1", Status: "COMPLETED"})
	}, 30*time.Second)
	f.RegisterDelayedCallback(func() {}, 10*time.Minute)
	f.run()
	if len(reasons) != 1 || reasons[0] != payment.ReviewPayoutUnconfirmed {
		t.Fatalf("reviews opened for %v, want PAYOUT_UNCONFIRMED (workflow error: %v)", reasons, f.GetWorkflowError())
	}
	if reviewAt < 0 || reviewAt > 3*time.Minute {
		t.Fatalf("the review opened %s after the start, want it within the 2-minute poll horizon and the wait for release", reviewAt)
	}
	f.require(map[string]int{"CommitPayout": 0, "ReturnPayout": 0})
}

func TestACancellationWhoseCompletionIsLostStillEndsOnceReturned(t *testing.T) {
	p := newPaymentFixture(t)
	id := p.payment.ID.String()
	lost := true
	f := newFlow(t, payment.Converted, map[string]stub{
		"DecideCommand": {value: func(ctx context.Context, paymentID, commandID string) (payment.Decision, error) {
			return p.acts.DecideCommand(ctx, paymentID, commandID)
		}},
		"CancelPayout": {value: func(ctx context.Context, paymentID, commandID string) error {
			if err := p.acts.CancelPayout(ctx, paymentID, commandID); err != nil {
				return err
			}
			if lost {
				lost = false
				return errors.New("the completion was lost")
			}
			return nil
		}},
	})
	f.in.PaymentID = id
	cmd, _, err := p.store.ClaimCommand(context.Background(), store.CommandClaim{
		ActorKind: "CUSTOMER", ActorID: "acme", RequestKey: "cancel", Operation: "REQUEST_CANCEL", SubjectID: id, Request: "cancel",
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	f.approve(t, cmd.ID.String(), 5*time.Second)
	f.run()
	f.requireEnded(payment.PayoutCancelled)
	if lost {
		t.Fatal("the cancellation never ran")
	}
	if n := p.ledger.count(); n != 1 {
		t.Errorf("%d returns posted, want 1", n)
	}
	if payout, status := p.statuses(); payout != "CANCELLED" || status != "PAYOUT_CANCELLED" {
		t.Errorf("the payout is %s and the payment %s", payout, status)
	}
}
