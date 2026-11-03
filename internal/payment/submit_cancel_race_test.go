package payment_test

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/xJohnNascimento/corridor/internal/payment"
	"github.com/xJohnNascimento/corridor/internal/store"
)

// Each round holds the payment's row locks while a PrepareSubmit and a cancel decision both queue
// for them, then sets the release time to land within a fraction of a millisecond of the moment the
// locks are let go. Which of the two takes the locks first, and whether the payout is due by then,
// varies from round to round; in every one exactly one of them wins.
func TestACancelAndASubmissionNeverBothWin(t *testing.T) {
	const rounds = 200
	var cancelled, submitted atomic.Int64
	t.Run("rounds", func(t *testing.T) {
		for round := range rounds {
			t.Run(fmt.Sprintf("round-%03d", round), func(t *testing.T) {
				t.Parallel()
				if raceRound(t, round) {
					cancelled.Add(1)
					return
				}
				submitted.Add(1)
			})
		}
	})
	t.Logf("%d rounds: %d cancelled, %d submitted", rounds, cancelled.Load(), submitted.Load())
}

// raceRound reports whether the cancel won.
func raceRound(t *testing.T, round int) bool {
	t.Helper()
	ctx := context.Background()
	rng := rand.New(rand.NewPCG(uint64(round), 1)) //nolint:gosec // reproducible timing jitter, not a secret
	releaseOffset := time.Duration(rng.IntN(6000)) * time.Microsecond
	head := time.Duration(rng.IntN(300)) * time.Microsecond
	cancelFirst := rng.IntN(2) == 0

	f := newPaymentFixture(t)
	paymentID := f.payment.ID.String()
	command, _, err := f.store.ClaimCommand(ctx, store.CommandClaim{
		ActorKind: "CUSTOMER", ActorID: "acme", RequestKey: "cancel", Operation: "REQUEST_CANCEL", SubjectID: paymentID, Request: "cancel",
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Owner.Exec(ctx, "update fx.payout set status = 'WAITING_RELEASE' where payment_id = $1", f.payment.ID); err != nil {
		t.Fatal(err)
	}

	hold, err := f.db.Owner.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = hold.Rollback(ctx) }()
	if _, err := hold.Exec(ctx, "select 1 from fx.payment p join fx.payout o on o.payment_id = p.id where p.id = $1 for update of p, o", f.payment.ID); err != nil {
		t.Fatal(err)
	}

	var (
		wg       sync.WaitGroup
		prepared payment.PrepareResult
		prepErr  error
		decision payment.Decision
		decErr   error
	)
	prepare := func() {
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			prepared, prepErr = f.acts.PrepareSubmit(ctx, paymentID)
			if prepErr != nil || prepared.Result != payment.SubmitEarly {
				return
			}
		}
		prepErr = errors.New("the payout was still not due after 10 seconds")
	}
	decide := func() { decision, decErr = f.acts.DecideCommand(ctx, paymentID, command.ID.String()) }
	first, second := prepare, decide
	if cancelFirst {
		first, second = decide, prepare
	}
	wg.Go(first)
	wg.Go(func() {
		time.Sleep(head)
		second()
	})

	waitForLockWaiters(t, f.db.Owner, 2)
	if _, err := hold.Exec(ctx, "update fx.payout set release_at = clock_timestamp() + $2::interval where payment_id = $1",
		f.payment.ID, fmt.Sprintf("%d microseconds", releaseOffset.Microseconds()),
	); err != nil {
		t.Fatal(err)
	}
	if err := hold.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if prepErr != nil || decErr != nil {
		t.Fatalf("prepare: %v, decide: %v", prepErr, decErr)
	}

	var (
		status         string
		cancelCommand  *uuid.UUID
		submissionSeen bool
	)
	err = f.db.Owner.QueryRow(ctx,
		"select status, cancel_command_id, submission_started_at is not null from fx.payout where payment_id = $1", f.payment.ID,
	).Scan(&status, &cancelCommand, &submissionSeen)
	if err != nil {
		t.Fatal(err)
	}

	if decision.State == payment.Accepted {
		if prepared.Result != payment.SubmitCancelled || prepared.CancelCommandID != command.ID.String() {
			t.Fatalf("the cancel was accepted but the submission answered %+v", prepared)
		}
		if status == string(payment.Submitting) || submissionSeen || cancelCommand == nil || *cancelCommand != command.ID {
			t.Fatalf("the cancel was accepted but the payout is %s, submission started %v, cancel command %v", status, submissionSeen, cancelCommand)
		}
		return true
	}
	if decision.State != "REJECTED" || decision.Reason != "cancel_window_closed" {
		t.Fatalf("the cancel was %s (%s), want ACCEPTED or REJECTED with cancel_window_closed", decision.State, decision.Reason)
	}
	if prepared.Result != payment.SubmitMoved || status != string(payment.Submitting) || !submissionSeen || cancelCommand != nil {
		t.Fatalf("the cancel was rejected but the submission answered %+v, the payout is %s, submission started %v, cancel command %v", prepared, status, submissionSeen, cancelCommand)
	}
	return false
}

// waitForLockWaiters returns once n sessions of the database wait on a row lock, so both
// contenders are queued before the lock is released.
func waitForLockWaiters(t *testing.T, owner *pgxpool.Pool, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var waiting int
		err := owner.QueryRow(context.Background(),
			"select count(*) from pg_stat_activity where datname = current_database() and wait_event_type = 'Lock'",
		).Scan(&waiting)
		if err != nil {
			t.Fatal(err)
		}
		if waiting >= n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("fewer than %d sessions waited on the row locks", n)
}
