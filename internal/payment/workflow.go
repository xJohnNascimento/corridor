package payment

import (
	"errors"
	"fmt"
	"slices"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/xJohnNascimento/corridor/internal/pricing"
	"github.com/xJohnNascimento/corridor/internal/provider"
)

// rejectedIsFinal gates counting a REJECTED provider answer to a hint as final, so histories
// recorded before the change replay as they ran.
const rejectedIsFinal = "payout-rejected-is-final"

// pollBreachReview gates opening the contract-breach review when the poll fails with a breach;
// before it, any poll failure was the horizon.
const pollBreachReview = "poll-contract-breach-review"

// exactTolerance gates comparing a fill with its quote exactly; before it, the comparison wrapped
// for an anomalously large fill and booked it.
const exactTolerance = "fill-tolerance-exact"

// asyncReadBack gates reading a hint back in a separate Activity that the selector races
// against the poll; before it, the hint's handler waited for the read-back and so held back the
// poll horizon.
const asyncReadBack = "hint-read-back-async"

type PaymentStatus string

const (
	Admitted        PaymentStatus = "ADMITTED"
	Converting      PaymentStatus = "CONVERTING"
	Converted       PaymentStatus = "CONVERTED"
	PayingOut       PaymentStatus = "PAYING_OUT"
	Completed       PaymentStatus = "COMPLETED"
	Rejected        PaymentStatus = "REJECTED"
	Expired         PaymentStatus = "EXPIRED"
	PayoutFailed    PaymentStatus = "PAYOUT_FAILED"
	PayoutCancelled PaymentStatus = "PAYOUT_CANCELLED"
)

// PayoutStatus is the status of the payout row, which is not the provider's status of the payout:
// the provider's are in package provider.
type PayoutStatus string

const (
	Planned         PayoutStatus = "PLANNED"
	Reserving       PayoutStatus = "RESERVING"
	WaitingFunds    PayoutStatus = "WAITING_FUNDS"
	WaitingRelease  PayoutStatus = "WAITING_RELEASE"
	Cancelling      PayoutStatus = "CANCELLING"
	Submitting      PayoutStatus = "SUBMITTING"
	Sent            PayoutStatus = "SENT"
	PayoutCompleted PayoutStatus = "COMPLETED"
	Failed          PayoutStatus = "FAILED"
	Cancelled       PayoutStatus = "CANCELLED"
	Skipped         PayoutStatus = "SKIPPED"
)

// Accepted is the state of a command that was decided in favour.
const Accepted = "ACCEPTED"

var (
	acts *Activities

	paymentStatusKey = temporal.NewSearchAttributeKeyKeyword("PaymentStatus")
	customerKey      = temporal.NewSearchAttributeKeyKeyword("CustomerId")
	inReviewKey      = temporal.NewSearchAttributeKeyBool("InReview")
	reviewSinceKey   = temporal.NewSearchAttributeKeyTime("ReviewSince")
)

func Terminal(status PaymentStatus) bool {
	return slices.Contains([]PaymentStatus{Completed, Rejected, Expired, PayoutFailed, PayoutCancelled}, status)
}

type state struct {
	in       Input
	snap     Snapshot
	cancel   string
	approval *Approval
	hints    workflow.ReceiveChannel
	seen     map[string]bool
}

func PaymentWorkflow(ctx workflow.Context, in Input) error {
	s := &state{in: in, seen: map[string]bool{}, hints: workflow.GetSignalChannel(ctx, SignalProviderHint)}
	if err := s.registerHandlers(ctx); err != nil {
		panic(fmt.Errorf("payment %s: %w", in.PaymentID, err))
	}
	if err := workflow.ExecuteActivity(s.db(ctx), acts.Load, in.PaymentID).Get(ctx, &s.snap); err != nil {
		panic(fmt.Errorf("payment %s: load: %w", in.PaymentID, err))
	}
	s.cancel = s.snap.CancelCommandID
	upsertSearchAttributes(ctx, customerKey.ValueSet(in.CustomerID), paymentStatusKey.ValueSet(string(s.snap.PaymentStatus)))

	err := replayBreak(ctx)
	if err == nil {
		err = s.run(ctx)
	}
	if temporal.IsCanceledError(err) {
		return s.markRecoveryRequired(ctx, err)
	}
	if err != nil {
		// A case the transition table does not cover is a bug: fail the workflow task, never the payment.
		panic(fmt.Errorf("payment %s: %w", in.PaymentID, err))
	}
	return s.finish(ctx)
}

func (s *state) registerHandlers(ctx workflow.Context) error {
	err := workflow.SetUpdateHandler(ctx, UpdateAwaitConversion, func(ctx workflow.Context) (string, error) {
		_, err := workflow.AwaitWithTimeout(ctx, s.in.Policy.FastPathWait, s.conversionDecided)
		return string(s.snap.PaymentStatus), err
	})
	if err != nil {
		return err
	}
	return workflow.SetUpdateHandlerWithOptions(ctx, UpdateExecuteCommand,
		func(ctx workflow.Context, commandID string) (Decision, error) {
			var d Decision
			if err := workflow.ExecuteActivity(s.db(ctx), acts.DecideCommand, s.in.PaymentID, commandID).Get(ctx, &d); err != nil {
				return d, err
			}
			if d.State == Accepted && d.Cancel {
				s.cancel = commandID
			}
			if d.State == Accepted && d.Approval != nil {
				s.approval = d.Approval
			}
			return d, nil
		},
		workflow.UpdateHandlerOptions{Validator: func(_ workflow.Context, commandID string) error {
			if commandID == "" {
				return errors.New("a command id is required")
			}
			return nil
		}},
	)
}

func (s *state) conversionDecided() bool {
	return Terminal(s.snap.PaymentStatus) || s.snap.PaymentStatus == Converted || s.snap.PaymentStatus == PayingOut || s.snap.ReviewReason != ""
}

// run advances the payment until it ends. A review stops it until an approved decision arrives;
// applying the decision moves it on from the same point.
func (s *state) run(ctx workflow.Context) error {
	for !Terminal(s.snap.PaymentStatus) {
		if s.snap.ReviewReason != "" {
			if err := s.resolveReview(ctx); err != nil {
				return err
			}
			continue
		}
		var err error
		switch s.snap.PaymentStatus {
		case Admitted, Converting:
			err = s.convert(ctx)
		case Converted, PayingOut:
			err = s.payout(ctx)
		default:
			err = fmt.Errorf("payment status %s has no step", s.snap.PaymentStatus)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *state) convert(ctx workflow.Context) error {
	if s.snap.PaymentStatus == Admitted {
		if s.tooLateToTrade(ctx) {
			return s.step(ctx, acts.ExpireConversion, nil, Expired)
		}
		var r Reserved
		if err := workflow.ExecuteActivity(s.money(ctx), acts.ReserveConversion, s.in.PaymentID).Get(ctx, &r); err != nil {
			return s.reviewOn(ctx, err)
		}
		if r.Refused {
			s.setStatus(ctx, Rejected)
			return nil
		}
		s.setStatus(ctx, Converting)
	}
	if !s.snap.SubmissionStarted && s.tooLateToTrade(ctx) {
		return s.step(ctx, acts.ReleaseConversion, nil, Expired)
	}

	out, err := s.trade(ctx)
	if err != nil {
		return s.reviewOn(ctx, err)
	}
	switch out.Status {
	case provider.TradeFilled:
		if !out.Matches {
			return s.openReview(ctx, ReviewProviderContractBreach)
		}
		if !s.withinTolerance(ctx, out.Delivered) {
			return s.openReview(ctx, ReviewFillOutOfTolerance)
		}
		return s.step(ctx, acts.CommitConversion, out.Delivered, Converted)
	case provider.TradeRejected, provider.TradeClosed:
		return s.step(ctx, acts.ReleaseConversion, nil, Expired)
	}
	return fmt.Errorf("trade outcome %s has no step", out.Status)
}

// withinTolerance asks for a version only when the exact comparison and the wrapping one it
// replaced disagree, so a history recorded before exactTolerance replays as it ran.
func (s *state) withinTolerance(ctx workflow.Context, delivered int64) bool {
	tolerance := pricing.Tolerance{Bps: s.in.Policy.FillToleranceBps}
	quoted := s.in.Quote.ProviderBuy
	exact := tolerance.Within(quoted, delivered)
	diff := delivered - quoted
	if diff < 0 {
		diff = -diff
	}
	wrapped := diff*10_000 <= quoted*tolerance.Bps
	if exact == wrapped || workflow.GetVersion(ctx, exactTolerance, workflow.DefaultVersion, 1) != workflow.DefaultVersion {
		return exact
	}
	return wrapped
}

func (s *state) tooLateToTrade(ctx workflow.Context) bool {
	return !workflow.Now(ctx).Before(s.in.Quote.ProviderExpiresAt.Add(-s.in.Policy.TradeSafety))
}

// trade submits until the provider quote expires, then resolves the reference: a lookup, and a
// close if it is still unknown. A second trade is never created to repair an unknown first one.
func (s *state) trade(ctx workflow.Context) (TradeOutcome, error) {
	var out TradeOutcome
	remaining := s.in.Quote.ProviderExpiresAt.Sub(workflow.Now(ctx))
	if remaining > 0 {
		s.snap.SubmissionStarted = true
		err := workflow.ExecuteActivity(s.provider(ctx, remaining, 10*time.Second), acts.SubmitTrade, s.in.PaymentID).Get(ctx, &out)
		var timeout *temporal.TimeoutError
		if err == nil || !errors.As(err, &timeout) {
			return out, err
		}
	}
	err := workflow.ExecuteActivity(s.provider(ctx, 0, time.Minute), acts.ResolveTrade, s.in.PaymentID).Get(ctx, &out)
	return out, err
}

func (s *state) payout(ctx workflow.Context) error {
	if s.snap.PayoutStatus == Planned {
		if s.cancel != "" {
			return s.cancelPayout(ctx)
		}
		if err := s.step(ctx, acts.StartPayout, nil, PayingOut); err != nil {
			return err
		}
		s.snap.PayoutStatus = Reserving
	}
	if s.snap.PayoutStatus == Reserving || s.snap.PayoutStatus == WaitingFunds {
		if err := s.reservePayout(ctx); err != nil || s.snap.PayoutStatus != WaitingRelease {
			return err
		}
	}
	if s.snap.PayoutStatus == WaitingRelease {
		if err := s.waitRelease(ctx); err != nil || s.snap.PayoutStatus != Submitting {
			return err
		}
	}
	if s.snap.PayoutStatus == Submitting {
		var out PayoutOutcome
		if err := workflow.ExecuteActivity(s.provider(ctx, 0, time.Minute), acts.SubmitPayout, s.in.PaymentID).Get(ctx, &out); err != nil {
			return s.reviewOn(ctx, err)
		}
		if finalPayout(out.Status) {
			return s.applyPayoutEvidence(ctx, out)
		}
		s.snap.PayoutStatus = Sent
	}
	if s.snap.PayoutStatus == Sent {
		return s.settle(ctx)
	}
	return fmt.Errorf("payout status %s has no step", s.snap.PayoutStatus)
}

// reservePayout reserves clearing and provider cash. A refusal on provider cash waits for funds and
// tries again under a new attempt; an accepted cancel is applied once the attempt's durable
// outcome is known, so exactly one return is ever posted.
func (s *state) reservePayout(ctx workflow.Context) error {
	for {
		if s.snap.PayoutStatus == WaitingFunds {
			if _, err := workflow.AwaitWithTimeout(ctx, s.in.Policy.LiquidityRetry, func() bool { return s.cancel != "" }); err != nil {
				workflow.GetLogger(ctx).Warn("wait for a cancel or funds", "error", err)
			}
			if s.cancel != "" {
				return s.cancelPayout(ctx)
			}
			if err := workflow.ExecuteActivity(s.db(ctx), acts.NextAttempt, s.in.PaymentID, s.snap.Attempt).Get(ctx, nil); err != nil {
				return err
			}
			s.snap.Attempt++
			s.snap.PayoutStatus = Reserving
		}
		var r Reserved
		if err := workflow.ExecuteActivity(s.money(ctx), acts.ReservePayout, s.in.PaymentID, s.snap.Attempt).Get(ctx, &r); err != nil {
			return s.reviewOn(ctx, err)
		}
		switch {
		case s.cancel != "":
			return s.cancelPayout(ctx)
		case r.Refused && r.OnProvider:
			if err := workflow.ExecuteActivity(s.db(ctx), acts.WaitFunds, s.in.PaymentID, s.snap.Attempt).Get(ctx, nil); err != nil {
				return err
			}
			s.snap.PayoutStatus = WaitingFunds
		case r.Refused:
			return s.openReview(ctx, ReviewLedgerUnexpected)
		default:
			var releaseAt time.Time
			if err := workflow.ExecuteActivity(s.db(ctx), acts.SetReleaseTime, s.in.PaymentID).Get(ctx, &releaseAt); err != nil {
				return err
			}
			s.snap.ReleaseAt, s.snap.PayoutStatus = &releaseAt, WaitingRelease
			return nil
		}
	}
}

// waitRelease holds the payout in its cancel window. PrepareSubmit decides on the database clock
// under the same row locks as a cancel decision, so a cancel and a submission never both win.
func (s *state) waitRelease(ctx workflow.Context) error {
	for {
		if wait := s.snap.ReleaseAt.Sub(workflow.Now(ctx)); wait > 0 {
			if _, err := workflow.AwaitWithTimeout(ctx, wait, func() bool { return s.cancel != "" }); err != nil {
				workflow.GetLogger(ctx).Warn("wait for a cancel or the release time", "error", err)
			}
		}
		if s.cancel != "" {
			return s.cancelPayout(ctx)
		}
		var result PrepareResult
		if err := workflow.ExecuteActivity(s.db(ctx), acts.PrepareSubmit, s.in.PaymentID).Get(ctx, &result); err != nil {
			return err
		}
		switch result.Result {
		case SubmitMoved:
			s.snap.PayoutStatus = Submitting
			return nil
		case SubmitCancelled:
			s.cancel = result.CancelCommandID
		case SubmitEarly:
			if err := workflow.Sleep(ctx, time.Second); err != nil {
				return err
			}
		}
	}
}

// settle races a retrying poll against provider hints. A hint moves money only after a read-back
// confirms it; the read-back runs beside the poll, so the poll horizon opens a review with the
// reservation intact even while a read-back keeps failing.
func (s *state) settle(ctx workflow.Context) error {
	pollCtx, cancelPoll := workflow.WithCancel(ctx)
	defer cancelPoll()
	readCtx, cancelReads := workflow.WithCancel(ctx)
	defer cancelReads()
	poll := workflow.ExecuteActivity(s.poll(pollCtx), acts.PollPayout, s.in.PaymentID)
	var readBacks []workflow.Future
	for {
		var (
			evidence *PayoutOutcome
			err      error
			horizon  bool
		)
		sel := workflow.NewSelector(ctx)
		sel.AddReceive(s.hints, func(c workflow.ReceiveChannel, _ bool) {
			var h Hint
			c.Receive(ctx, &h)
			duplicate := s.seen[h.EventID]
			s.seen[h.EventID] = true
			if duplicate || !finalHint(ctx, h.Status) {
				return
			}
			if workflow.GetVersion(ctx, asyncReadBack, workflow.DefaultVersion, 1) == workflow.DefaultVersion {
				var out PayoutOutcome
				err = workflow.ExecuteActivity(s.provider(ctx, 0, time.Minute), acts.VerifyPayout, s.in.PaymentID, h).Get(ctx, &out)
				if err == nil && (finalHint(ctx, out.Status) || !out.Matches) {
					evidence = &out
				}
				return
			}
			readBacks = append(readBacks, workflow.ExecuteActivity(s.provider(readCtx, 0, time.Minute), acts.VerifyPayout, s.in.PaymentID, h))
		})
		for _, readBack := range readBacks {
			sel.AddFuture(readBack, func(f workflow.Future) {
				readBacks = slices.DeleteFunc(readBacks, func(pending workflow.Future) bool { return pending == f })
				var out PayoutOutcome
				if err = f.Get(ctx, &out); err == nil && (finalHint(ctx, out.Status) || !out.Matches) {
					evidence = &out
				}
			})
		}
		sel.AddFuture(poll, func(f workflow.Future) {
			var out PayoutOutcome
			if pollErr := f.Get(ctx, &out); pollErr != nil {
				if isContractBreach(pollErr) && workflow.GetVersion(ctx, pollBreachReview, workflow.DefaultVersion, 1) == 1 {
					err = pollErr
					return
				}
				horizon = true
				return
			}
			evidence = &out
		})
		sel.Select(ctx)
		switch {
		case err != nil:
			cancelReads()
			return s.reviewOn(ctx, err)
		case evidence != nil:
			cancelPoll()
			cancelReads()
			return s.applyPayoutEvidence(ctx, *evidence)
		case horizon:
			cancelReads()
			return s.openReview(ctx, ReviewPayoutUnconfirmed)
		}
	}
}

func (s *state) applyPayoutEvidence(ctx workflow.Context, ev PayoutOutcome) error {
	if !ev.Matches {
		return s.openReview(ctx, ReviewConflictingProviderEvidence)
	}
	if ev.Status == provider.PayoutCompleted {
		return s.step(ctx, acts.CommitPayout, ev, Completed)
	}
	return s.step(ctx, acts.ReturnPayout, ev, PayoutFailed)
}

func (s *state) cancelPayout(ctx workflow.Context) error {
	return s.step(ctx, acts.CancelPayout, s.cancel, PayoutCancelled)
}

// step runs a money activity that writes its own transitions after the ledger answers, then
// mirrors the payment status it reached.
func (s *state) step(ctx workflow.Context, activity any, arg any, status PaymentStatus) error {
	args := []any{s.in.PaymentID}
	if arg != nil {
		args = append(args, arg)
	}
	if err := workflow.ExecuteActivity(s.money(ctx), activity, args...).Get(ctx, nil); err != nil {
		return s.reviewOn(ctx, err)
	}
	s.setStatus(ctx, status)
	return nil
}

func (s *state) setStatus(ctx workflow.Context, status PaymentStatus) {
	s.snap.PaymentStatus = status
	upsertSearchAttributes(ctx, paymentStatusKey.ValueSet(string(status)))
}

func upsertSearchAttributes(ctx workflow.Context, updates ...temporal.SearchAttributeUpdate) {
	if err := workflow.UpsertTypedSearchAttributes(ctx, updates...); err != nil {
		workflow.GetLogger(ctx).Warn("upsert search attributes", "error", err)
	}
}

func isContractBreach(err error) bool {
	var app *temporal.ApplicationError
	return errors.As(err, &app) && app.Type() == ErrorContractBreach
}

// reviewOn turns a covered domain error into a review and returns any other error unchanged.
func (s *state) reviewOn(ctx workflow.Context, err error) error {
	var app *temporal.ApplicationError
	if errors.As(err, &app) && (app.Type() == ErrorLedgerUnexpected || app.Type() == ErrorContractBreach) {
		return s.openReview(ctx, ReviewReason(app.Type()))
	}
	return err
}

func (s *state) openReview(ctx workflow.Context, reason ReviewReason) error {
	if err := workflow.ExecuteActivity(s.db(ctx), acts.OpenReview, s.in.PaymentID, reason).Get(ctx, nil); err != nil {
		return err
	}
	s.snap.ReviewReason = reason
	upsertSearchAttributes(ctx, inReviewKey.ValueSet(true), reviewSinceKey.ValueSet(workflow.Now(ctx)))
	return nil
}

// resolveReview waits for an approved decision and applies it. Applying always checks the
// evidence again first; a contradiction keeps the review open.
func (s *state) resolveReview(ctx workflow.Context) error {
	if err := workflow.Await(ctx, func() bool { return s.approval != nil }); err != nil {
		return err
	}
	approval := *s.approval
	s.approval = nil
	var applied Applied
	if err := workflow.ExecuteActivity(s.money(ctx), acts.ApplyApproval, s.in.PaymentID, approval).Get(ctx, &applied); err != nil {
		return err
	}
	if !applied.Done {
		return nil
	}
	s.snap.ReviewReason = ""
	s.snap.PayoutStatus = applied.PayoutStatus
	s.setStatus(ctx, applied.PaymentStatus)
	upsertSearchAttributes(ctx, inReviewKey.ValueSet(false))
	return nil
}

func (s *state) markRecoveryRequired(ctx workflow.Context, cause error) error {
	cleanup, _ := workflow.NewDisconnectedContext(ctx)
	if err := workflow.ExecuteActivity(s.db(cleanup), acts.MarkRecoveryRequired, s.in.PaymentID).Get(cleanup, nil); err != nil {
		workflow.GetLogger(ctx).Error("mark the payment as needing recovery", "error", err)
	}
	return cause
}

// finish settles a cancel that had nothing left to cancel, drains hints and lets every in-flight
// handler answer before the workflow closes.
func (s *state) finish(ctx workflow.Context) error {
	if s.cancel != "" {
		if err := workflow.ExecuteActivity(s.db(ctx), acts.ApplyCommand, s.cancel).Get(ctx, nil); err != nil {
			return err
		}
	}
	var h Hint
	for s.hints.ReceiveAsync(&h) {
		workflow.GetLogger(ctx).Info("hint after the payment ended", "event", h.EventID, "status", h.Status)
	}
	return workflow.Await(ctx, func() bool { return workflow.AllHandlersFinished(ctx) })
}

// finalPayout reports whether a provider's payout status can no longer change.
func finalPayout(status string) bool {
	switch status {
	case provider.PayoutCompleted, provider.PayoutFailed, provider.PayoutCancelled, provider.PayoutRejected:
		return true
	}
	return false
}

// finalHint is finalPayout for an answer received while settling.
func finalHint(ctx workflow.Context, status string) bool {
	if status == provider.PayoutRejected && workflow.GetVersion(ctx, rejectedIsFinal, workflow.DefaultVersion, 1) == workflow.DefaultVersion {
		return false
	}
	return finalPayout(status)
}

func (s *state) db(ctx workflow.Context) workflow.Context {
	return workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		TaskQueue:           TaskQueue,
		StartToCloseTimeout: 5 * time.Second,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval: 200 * time.Millisecond, BackoffCoefficient: 2, MaximumInterval: 30 * time.Second,
			NonRetryableErrorTypes: []string{ErrorTransitionConflict},
		},
	})
}

// money has no schedule-to-close: it waits for the ledger for as long as it takes. Alerting on a
// payment that waits too long belongs to the observability profile.
func (s *state) money(ctx workflow.Context) workflow.Context {
	return workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		TaskQueue:           TaskQueue,
		StartToCloseTimeout: 5 * time.Second,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval: 500 * time.Millisecond, BackoffCoefficient: 2, MaximumInterval: 30 * time.Second,
			NonRetryableErrorTypes: []string{ErrorLedgerUnexpected, ErrorContractBreach, ErrorTransitionConflict},
		},
	})
}

func (s *state) provider(ctx workflow.Context, scheduleToClose, maxInterval time.Duration) workflow.Context {
	return workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		TaskQueue:              ProviderTaskQueue,
		StartToCloseTimeout:    10 * time.Second,
		ScheduleToCloseTimeout: scheduleToClose,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval: time.Second, BackoffCoefficient: 2, MaximumInterval: maxInterval,
			NonRetryableErrorTypes: []string{ErrorContractBreach},
		},
	})
}

// poll retries a read until the payout is terminal: the retries are the polling loop, so waiting
// adds no history.
func (s *state) poll(ctx workflow.Context) workflow.Context {
	return workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		TaskQueue:              ProviderTaskQueue,
		StartToCloseTimeout:    10 * time.Second,
		ScheduleToCloseTimeout: s.in.Policy.PollHorizon,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval: s.in.Policy.PollInitial, BackoffCoefficient: 2, MaximumInterval: s.in.Policy.PollMax,
		},
	})
}
