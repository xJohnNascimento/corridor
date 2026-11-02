package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	corridorv1 "github.com/xJohnNascimento/corridor/api/corridor/v1"
)

func insufficientFunds(ctx context.Context, s *stack) error {
	_, p, err := s.settle(ctx, plan{prefix: "s02", sell: "CAD", buy: "USD", amount: 100000, funds: 5000}, time.Minute)
	if err != nil {
		return err
	}
	reservation, err := s.reservationState(ctx, "corridor.conversion", p.Id)
	if err != nil {
		return err
	}
	_, traded, err := s.dittoTrade(ctx, p.Id)
	if err != nil {
		return err
	}
	return errors.Join(
		expect("status", status(p), "REJECTED"),
		expect("reason", p.OutcomeReason, corridorv1.OutcomeReason_OUTCOME_REASON_INSUFFICIENT_FUNDS),
		expect("conversion reservation", reservation, "NONE"),
		expect("a trade exists", traded, false),
	)
}

func tradeRejected(ctx context.Context, s *stack) error {
	request := cadToUsd("s03")
	request.beforePay = func(q *corridorv1.Quote) error {
		return s.armFault(ctx, "TRADE_REJECT", "quote", q.Id, nil)
	}
	c, p, err := s.settle(ctx, request, time.Minute)
	if err != nil {
		return err
	}
	reservation, err := s.reservationState(ctx, "corridor.conversion", p.Id)
	if err != nil {
		return err
	}
	left, err := s.funds(ctx, c, "CAD")
	if err != nil {
		return err
	}
	return errors.Join(
		expect("status", status(p), "EXPIRED"),
		expect("reason", p.OutcomeReason, corridorv1.OutcomeReason_OUTCOME_REASON_RATE_UNAVAILABLE),
		expect("conversion reservation", reservation, "RESERVATION_STATE_RELEASED"),
		expect("customer CAD", left, quoteSell),
	)
}

func payoutFailed(ctx context.Context, s *stack) error {
	c, paid, err := s.converted(ctx, "s04", nil)
	if err != nil {
		return err
	}
	if err := s.armFault(ctx, "PAYOUT_FAIL", "payout", paid.Receipt.PayoutId, map[string]any{"code": "AC04"}); err != nil {
		return err
	}
	p, err := c.waitFor(ctx, paid.Receipt.PaymentId, 3*time.Minute, ended)
	if err != nil {
		return err
	}
	usd, err := s.funds(ctx, c, "USD")
	if err != nil {
		return err
	}
	return errors.Join(
		expect("status", status(p), "PAYOUT_FAILED"),
		expect("reason", p.OutcomeReason, corridorv1.OutcomeReason_OUTCOME_REASON_BENEFICIARY_ACCOUNT_CLOSED),
		expect("customer USD after one return", usd, customerBuy),
		expect("returned", p.Returned.GetAmount(), fmt.Sprint(customerBuy)),
	)
}

func cancelInWindow(ctx context.Context, s *stack) error {
	c, paid, err := s.converted(ctx, "s05", payoutIs(corridorv1.PayoutStatus_PAYOUT_STATUS_WAITING_RELEASE))
	if err != nil {
		return err
	}
	time.Sleep(5 * time.Second)
	command, err := c.cancel(ctx, paid.Receipt.PaymentId)
	if err != nil {
		return err
	}
	p, err := c.waitFor(ctx, paid.Receipt.PaymentId, time.Minute, ended)
	if err != nil {
		return err
	}
	_, submitted, err := s.dittoPayout(ctx, paid.Receipt.PayoutId)
	if err != nil {
		return err
	}
	usd, err := s.funds(ctx, c, "USD")
	if err != nil {
		return err
	}
	applied, err := c.command(ctx, command.Id)
	if err != nil {
		return err
	}
	return errors.Join(
		expect("decision", command.State, corridorv1.CommandState_COMMAND_STATE_ACCEPTED),
		expect("status", status(p), "PAYOUT_CANCELLED"),
		expect("submitted", submitted, false),
		expect("customer USD", usd, customerBuy),
		expect("command", applied.State, corridorv1.CommandState_COMMAND_STATE_APPLIED),
	)
}

func ledgerReplyLost(ctx context.Context, s *stack) error {
	request := cadToUsd("s08")
	request.beforePay = func(*corridorv1.Quote) error {
		_, err := s.fx(ctx, "failpoints", "arm", "--name", "ledger.afterCommit.dropReply", "--uses", "3")
		return err
	}
	c, p, err := s.settle(ctx, request, 3*time.Minute)
	if err != nil {
		return err
	}
	cad, err := s.funds(ctx, c, "CAD")
	if err != nil {
		return err
	}
	usd, err := s.funds(ctx, c, "USD")
	if err != nil {
		return err
	}
	return errors.Join(
		expect("status", status(p), "COMPLETED"),
		expect("customer CAD", cad, 0),
		expect("customer USD", usd, 0),
	)
}

func restartWorker(ctx context.Context, s *stack) error {
	if err := s.composeRun(ctx, "kill", "fx-worker"); err != nil {
		return err
	}
	return s.composeRun(ctx, "start", "fx-worker")
}

func tradeReplyLost(ctx context.Context, s *stack) error {
	request := cadToUsd("s09")
	request.beforePay = func(q *corridorv1.Quote) error {
		return s.armFault(ctx, "TRADE_COMMIT_DROP_REPLY", "quote", q.Id, nil)
	}
	c, paid, err := s.start(ctx, request)
	if err != nil {
		return err
	}
	if err := restartWorker(ctx, s); err != nil {
		return err
	}
	p, err := c.waitFor(ctx, paid.Receipt.PaymentId, 3*time.Minute, ended)
	if err != nil {
		return err
	}
	trade, _, err := s.dittoTrade(ctx, p.Id)
	if err != nil {
		return err
	}
	cad, err := s.funds(ctx, c, "CAD")
	if err != nil {
		return err
	}
	return errors.Join(
		expect("status", status(p), "COMPLETED"),
		expect("trade", trade, "FILLED"),
		expect("customer CAD", cad, 0),
	)
}

func payoutReplyLost(ctx context.Context, s *stack) error {
	c, paid, err := s.converted(ctx, "s10", payoutIs(corridorv1.PayoutStatus_PAYOUT_STATUS_WAITING_RELEASE))
	if err != nil {
		return err
	}
	if err := s.armFault(ctx, "PAYOUT_COMMIT_DROP_REPLY", "payout", paid.Receipt.PayoutId, nil); err != nil {
		return err
	}
	if _, err := c.waitFor(ctx, paid.Receipt.PaymentId, time.Minute, payoutIs(corridorv1.PayoutStatus_PAYOUT_STATUS_SUBMITTING, corridorv1.PayoutStatus_PAYOUT_STATUS_SENT)); err != nil {
		return err
	}
	if err := restartWorker(ctx, s); err != nil {
		return err
	}
	p, err := c.waitFor(ctx, paid.Receipt.PaymentId, 3*time.Minute, ended)
	if err != nil {
		return err
	}
	payout, _, err := s.dittoPayout(ctx, paid.Receipt.PayoutId)
	if err != nil {
		return err
	}
	return errors.Join(
		expect("status", status(p), "COMPLETED"),
		expect("provider payout", payout.Status, "COMPLETED"),
	)
}

func hints(ctx context.Context, s *stack) error {
	c, paid, err := s.converted(ctx, "s11", payoutIs(corridorv1.PayoutStatus_PAYOUT_STATUS_WAITING_RELEASE))
	if err != nil {
		return err
	}
	for _, fault := range []string{"WEBHOOK_DUPLICATE", "WEBHOOK_REORDER", "WEBHOOK_OMIT", "FALSE_FAILED_HINT"} {
		if err := s.armFault(ctx, fault, "payout", paid.Receipt.PayoutId, nil); err != nil {
			return err
		}
	}
	p, err := c.waitFor(ctx, paid.Receipt.PaymentId, 3*time.Minute, ended)
	if err != nil {
		return err
	}
	usd, err := s.funds(ctx, c, "USD")
	if err != nil {
		return err
	}
	return errors.Join(
		expect("status", status(p), "COMPLETED"),
		expect("returned", p.Returned.GetAmount(), ""),
		expect("customer USD", usd, 0),
	)
}

// cancelRacesSubmission cancels exactly at the release time, three times: each payment either
// accepts the cancel and never submits, or refuses it and pays normally.
func cancelRacesSubmission(ctx context.Context, s *stack) error {
	for round := range 3 {
		c, paid, err := s.converted(ctx, fmt.Sprintf("s12-%d", round), payoutIs(corridorv1.PayoutStatus_PAYOUT_STATUS_WAITING_RELEASE))
		if err != nil {
			return err
		}
		p, err := c.payment(ctx, paid.Receipt.PaymentId)
		if err != nil {
			return err
		}
		time.Sleep(time.Until(p.Payout.ReleaseAt.AsTime()))
		command, err := c.cancel(ctx, p.Id)
		if err != nil {
			return err
		}
		if p, err = c.waitFor(ctx, p.Id, 3*time.Minute, ended); err != nil {
			return err
		}
		_, submitted, err := s.dittoPayout(ctx, paid.Receipt.PayoutId)
		if err != nil {
			return err
		}
		accepted := command.State == corridorv1.CommandState_COMMAND_STATE_ACCEPTED || command.State == corridorv1.CommandState_COMMAND_STATE_APPLIED
		switch {
		case accepted && (submitted || status(p) != "PAYOUT_CANCELLED"):
			return fmt.Errorf("round %d: cancel accepted but the payout was submitted (%v) or ended %s", round, submitted, status(p))
		case !accepted && status(p) != "COMPLETED":
			return fmt.Errorf("round %d: cancel refused (%s) but the payment ended %s", round, command.Reason, status(p))
		}
	}
	return nil
}

// lateCreateDelay outlasts every attempt's timeout and the quote itself.
const lateCreateDelay = 75 * time.Second

// lateCreate holds every create of the trade at the provider, so each attempt is still waiting
// when Corridor stops submitting, finds the reference unknown and closes it. The provider's own
// record must show that creates were held, and the outcome must be the fence's: the payment
// expired, the trade stays CLOSED and the customer has the sell amount back. A fill here would
// mean the creates were not held, or that the fence did not hold.
func lateCreate(ctx context.Context, s *stack) error {
	var quote string
	request := cadToUsd("s13")
	request.beforePay = func(q *corridorv1.Quote) error {
		quote = q.Id
		return s.armFaultTimes(ctx, "DELAY_BEFORE_EXECUTION", "quote", q.Id, map[string]any{"delay_ms": lateCreateDelay.Milliseconds()}, 20)
	}
	c, p, err := s.settle(ctx, request, 3*time.Minute)
	if err != nil {
		return err
	}
	held, err := s.faultFired(ctx, "DELAY_BEFORE_EXECUTION", "quote", quote)
	if err != nil {
		return err
	}
	trade, _, err := s.dittoTrade(ctx, p.Id)
	if err != nil {
		return err
	}
	cad, err := s.funds(ctx, c, "CAD")
	if err != nil {
		return err
	}
	return errors.Join(
		expect("status", status(p), "EXPIRED"),
		expect("provider trade", trade, "CLOSED"),
		expect("customer CAD", cad, quoteSell),
		expect("creates held at the provider", held > 0, true),
	)
}

// liquidityWait converts two payments, holds both before their payout reservation, drains provider
// USD so that only one fits, then refunds: the second waits for funds and both complete.
func liquidityWait(ctx context.Context, s *stack) error {
	if _, err := s.fx(ctx, "failpoints", "arm", "--name", "payout.beforeReserve.hold"); err != nil {
		return err
	}
	var payments []*corridorv1.CreatePaymentResponse
	var customers []*customer
	for i := range 2 {
		c, paid, err := s.converted(ctx, fmt.Sprintf("s14-%d", i), payoutIs(corridorv1.PayoutStatus_PAYOUT_STATUS_RESERVING))
		if err != nil {
			return err
		}
		customers, payments = append(customers, c), append(payments, paid)
	}
	reference := "s14-drain-" + randomHex(4)
	drained, err := s.drain(ctx, "USD", customerBuy+100000, reference)
	if err != nil {
		return err
	}
	if _, err := s.fx(ctx, "failpoints", "release", "--name", "payout.beforeReserve.hold"); err != nil {
		return err
	}
	waited := 0
	for i, c := range customers {
		p, err := c.waitFor(ctx, payments[i].Receipt.PaymentId, 30*time.Second, payoutIs(corridorv1.PayoutStatus_PAYOUT_STATUS_WAITING_FUNDS, corridorv1.PayoutStatus_PAYOUT_STATUS_WAITING_RELEASE))
		if err != nil {
			return err
		}
		if p.Payout.Status == corridorv1.PayoutStatus_PAYOUT_STATUS_WAITING_FUNDS {
			waited++
		}
	}
	if err := s.sweep(ctx, reference+"-refund", "USD", drained); err != nil {
		return err
	}
	if waited != 1 {
		return fmt.Errorf("%d payouts waited for funds, want 1", waited)
	}
	for i, c := range customers {
		p, err := c.waitFor(ctx, payments[i].Receipt.PaymentId, 3*time.Minute, ended)
		if err != nil {
			return err
		}
		if status(p) != "COMPLETED" {
			return fmt.Errorf("payment %d ended %s", i, status(p))
		}
	}
	return nil
}

// cancelDuringLiquidityWait refuses attempt 1 for funds, refunds, holds the ledger's reply to
// attempt 2, cancels, then releases the reply: the reserve outcome must be resolved first and
// exactly one return posted.
func cancelDuringLiquidityWait(ctx context.Context, s *stack) error {
	if _, err := s.fx(ctx, "failpoints", "arm", "--name", "payout.beforeReserve.hold"); err != nil {
		return err
	}
	c, paid, err := s.converted(ctx, "s15", payoutIs(corridorv1.PayoutStatus_PAYOUT_STATUS_RESERVING))
	if err != nil {
		return err
	}
	reference := "s15-drain-" + randomHex(4)
	drained, err := s.drain(ctx, "USD", customerBuy-1, reference)
	if err != nil {
		return err
	}
	if _, err := s.fx(ctx, "failpoints", "release", "--name", "payout.beforeReserve.hold"); err != nil {
		return err
	}
	if _, err := c.waitFor(ctx, paid.Receipt.PaymentId, 30*time.Second, payoutIs(corridorv1.PayoutStatus_PAYOUT_STATUS_WAITING_FUNDS)); err != nil {
		return err
	}
	if _, err := s.fx(ctx, "failpoints", "arm", "--name", "ledger.afterCommit.holdReply", "--subject", paid.Receipt.PaymentId); err != nil {
		return err
	}
	if err := s.sweep(ctx, reference+"-refund", "USD", drained); err != nil {
		return err
	}
	if err := s.waitHeld(ctx, "ledger.afterCommit.holdReply"); err != nil {
		return err
	}
	command, err := c.cancel(ctx, paid.Receipt.PaymentId)
	if err != nil {
		return err
	}
	if _, err := s.fx(ctx, "failpoints", "release", "--name", "ledger.afterCommit.holdReply"); err != nil {
		return err
	}
	p, err := c.waitFor(ctx, paid.Receipt.PaymentId, 2*time.Minute, ended)
	if err != nil {
		return err
	}
	usd, err := s.funds(ctx, c, "USD")
	if err != nil {
		return err
	}
	reservation, err := s.reservationState(ctx, "corridor.payout", paid.Receipt.PayoutId)
	if err != nil {
		return err
	}
	return errors.Join(
		expect("decision", command.State, corridorv1.CommandState_COMMAND_STATE_ACCEPTED),
		expect("status", status(p), "PAYOUT_CANCELLED"),
		expect("customer USD after one return", usd, customerBuy),
		expect("payout reservation", reservation, "RESERVATION_STATE_RELEASED"),
	)
}

func fillDifferences(ctx context.Context, s *stack) error {
	for _, delta := range []int64{-withinTolerance, withinTolerance} {
		house, err := s.houseAccounts(ctx, "CAD", "USD")
		if err != nil {
			return err
		}
		before, err := s.balances(ctx, house)
		if err != nil {
			return err
		}
		request := cadToUsd("s16")
		request.beforePay = func(q *corridorv1.Quote) error {
			return s.armFault(ctx, "FX_FILL_DIFFERENCE", "quote", q.Id, map[string]any{"delta": fmt.Sprint(delta)})
		}
		_, p, err := s.settle(ctx, request, 3*time.Minute)
		if err != nil {
			return err
		}
		after, err := s.balances(ctx, house)
		if err != nil {
			return err
		}
		if err := errors.Join(
			expect("status", status(p), "COMPLETED"),
			expect("customer amount", p.Buy.Amount, fmt.Sprint(customerBuy)),
			expect(fmt.Sprintf("difference account with delta %d", delta), after["diff USD"]-before["diff USD"], -delta),
			expect(fmt.Sprintf("provider USD with delta %d", delta), after["cash USD"]-before["cash USD"], providerBuy+delta-customerBuy),
		); err != nil {
			return err
		}
	}
	return nil
}
