package failpoint

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	AdmissionAfterCommit = "admission.afterCommit"
	LedgerDropReply      = "ledger.afterCommit.dropReply"
	LedgerHoldReply      = "ledger.afterCommit.holdReply"
	PayoutBeforeReserve  = "payout.beforeReserve.hold"
	CommandAfterAccepted = "command.afterAccepted"
	holdPoll             = 200 * time.Millisecond
)

var ErrInjected = errors.New("failpoint injected")

// Points arms named failures in the database so a scenario can trigger them in another process.
// A nil Points is the production build: every point is a no-op.
type Points struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Points {
	return &Points{pool: pool}
}

// Hit consumes one use of an armed point for the subject, or for any subject when it was armed
// without one.
func (p *Points) Hit(ctx context.Context, name, subject string) bool {
	if p == nil {
		return false
	}
	tag, err := p.pool.Exec(ctx, `
		update fx.failpoint_arm set remaining = remaining - 1
		where name = $1 and remaining > 0 and (subject = '' or subject = $2)`,
		name, subject,
	)
	if err != nil {
		slog.ErrorContext(ctx, "failpoint", "name", name, "error", err)
		return false
	}
	return tag.RowsAffected() == 1
}

// Hold blocks while the point is armed for the subject, until a scenario releases it. Every
// attempt waits, so an activity retried after a timeout is still caught at the same boundary.
func (p *Points) Hold(ctx context.Context, name, subject string) error {
	if p == nil {
		return nil
	}
	tag, err := p.pool.Exec(ctx, `
		update fx.failpoint_arm set held = true
		where name = $1 and remaining > 0 and (subject = '' or subject = $2)`,
		name, subject,
	)
	if err != nil || tag.RowsAffected() == 0 {
		return err
	}
	for {
		var armed bool
		if err := p.pool.QueryRow(ctx, "select remaining > 0 from fx.failpoint_arm where name = $1", name).Scan(&armed); err != nil || !armed {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(holdPoll):
		}
	}
}

func (p *Points) Arm(ctx context.Context, name, subject string, uses int) error {
	_, err := p.pool.Exec(ctx, `
		insert into fx.failpoint_arm (name, subject, remaining) values ($1, $2, $3)
		on conflict (name) do update set subject = excluded.subject, remaining = excluded.remaining, held = false, armed_at = now()`,
		name, subject, uses,
	)
	return err
}

// Release disarms a hold point and lets every caller waiting on it go on.
func (p *Points) Release(ctx context.Context, name string) error {
	_, err := p.pool.Exec(ctx, "update fx.failpoint_arm set held = false, remaining = 0 where name = $1", name)
	return err
}

func (p *Points) Held(ctx context.Context, name string) (bool, error) {
	var held bool
	err := p.pool.QueryRow(ctx, "select coalesce((select held from fx.failpoint_arm where name = $1), false)", name).Scan(&held)
	return held, err
}

// Remaining is how many uses of the point are still armed; a scenario reads it to prove a point
// fired.
func (p *Points) Remaining(ctx context.Context, name string) (int, error) {
	var remaining int
	err := p.pool.QueryRow(ctx, "select coalesce((select remaining from fx.failpoint_arm where name = $1), 0)", name).Scan(&remaining)
	return remaining, err
}
