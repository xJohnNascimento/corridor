package dispatch

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
	"golang.org/x/sync/errgroup"

	"github.com/xJohnNascimento/corridor/internal/payment"
	"github.com/xJohnNascimento/corridor/internal/store"
)

const (
	defaultLease = 30 * time.Second

	// A lane claims no more rows than it handles at once, so every row is handled as soon as its
	// lease is committed and the lease only has to cover one call and a margin.
	laneSize = 8

	signalled = "SIGNALLED"
	noTarget  = "NO_TARGET"
)

// Dispatcher guarantees that every admitted payment gets its workflow, every command reaches its
// workflow and every webhook hint is signalled. Starts, commands and hints are independent lanes,
// so a backlog in one never delays another. A lane leases rows with SKIP LOCKED, commits the
// lease and only then calls Temporal, so any number of replicas can run.
type Dispatcher struct {
	Store    *store.Store
	Temporal client.Client
	// Lease is how long a claimed row stays out of other dispatchers' reach; zero means the default.
	Lease time.Duration
}

func (d *Dispatcher) lease() time.Duration {
	if d.Lease == 0 {
		return defaultLease
	}
	return d.Lease
}

// callTimeout ends a call to Temporal before half the lease does, so one unreachable workflow
// cannot hold the rest of a batch past the lease that protects it.
func (d *Dispatcher) callTimeout() time.Duration {
	return d.lease() / 2
}

func (d *Dispatcher) Run(ctx context.Context, every time.Duration) {
	lanes := []struct {
		name string
		pass func(context.Context) (int, error)
	}{
		{"starts", d.starts},
		{"commands", d.commands},
		{"hints", d.hints},
	}
	var wg sync.WaitGroup
	for _, lane := range lanes {
		wg.Go(func() { d.loop(ctx, every, lane.name, lane.pass) })
	}
	wg.Wait()
}

// loop runs a lane's passes, back to back while they come back full and otherwise once per tick.
// A row that fails stays leased until its lease expires, so a full pass never turns into a spin.
func (d *Dispatcher) loop(ctx context.Context, every time.Duration, name string, pass func(context.Context) (int, error)) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		claimed, err := pass(ctx)
		if err != nil && ctx.Err() == nil {
			slog.ErrorContext(ctx, "dispatch", "loop", name, "error", err)
		}
		if err == nil && claimed == laneSize {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// handle runs fn on every claimed row at once, within the lane's bound, and waits for them all.
func handle[T any](ctx context.Context, rows []T, fn func(context.Context, T)) {
	var group errgroup.Group
	group.SetLimit(laneSize)
	for _, row := range rows {
		group.Go(func() error {
			fn(ctx, row)
			return nil
		})
	}
	_ = group.Wait()
}

func (d *Dispatcher) starts(ctx context.Context) (int, error) {
	ids, err := d.Store.LeasePendingStarts(ctx, laneSize, d.lease())
	if err != nil {
		return 0, err
	}
	handle(ctx, ids, func(ctx context.Context, id uuid.UUID) {
		if err := d.start(ctx, id); err != nil {
			slog.WarnContext(ctx, "start payment workflow", "payment", id, "error", err)
		}
	})
	return len(ids), nil
}

// start starts the payment's workflow, or finds it already started by the fast path or an earlier
// attempt; both mark the start done.
func (d *Dispatcher) start(ctx context.Context, id uuid.UUID) error {
	p, err := d.Store.Payment(ctx, id)
	if err != nil {
		return err
	}
	in, err := payment.InputFrom(p)
	if err != nil {
		return err
	}
	callCtx, cancel := context.WithTimeout(ctx, d.callTimeout())
	defer cancel()
	_, err = d.Temporal.ExecuteWorkflow(callCtx, client.StartWorkflowOptions{
		ID:                       payment.WorkflowID(id.String()),
		TaskQueue:                payment.TaskQueue,
		WorkflowIDReusePolicy:    enumspb.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE,
		WorkflowIDConflictPolicy: enumspb.WORKFLOW_ID_CONFLICT_POLICY_USE_EXISTING,
	}, payment.PaymentWorkflow, in)
	var started *serviceerror.WorkflowExecutionAlreadyStarted
	if err != nil && !errors.As(err, &started) {
		return err
	}
	return d.Store.ClearStartPending(ctx, id)
}

func (d *Dispatcher) commands(ctx context.Context) (int, error) {
	pendings, err := d.Store.LeaseCommands(ctx, laneSize, d.lease())
	if err != nil {
		return 0, err
	}
	handle(ctx, pendings, func(ctx context.Context, c store.PendingCommand) {
		if err := d.deliver(ctx, c.PaymentID, c.CommandID); err != nil {
			slog.WarnContext(ctx, "deliver command", "command", c.CommandID, "error", err)
		}
	})
	return len(pendings), nil
}

// deliver sends a command to its workflow. A closed workflow cannot decide it, so the command gets
// a durable REJECTED instead of staying RECEIVED forever.
func (d *Dispatcher) deliver(ctx context.Context, paymentID, commandID string) error {
	callCtx, cancel := context.WithTimeout(ctx, d.callTimeout())
	defer cancel()
	_, err := Deliver(callCtx, d.Temporal, paymentID, commandID)
	var notFound *serviceerror.NotFound
	if !errors.As(err, &notFound) {
		return err
	}
	_, err = d.Store.Pool().Exec(ctx, `
		update fx.product_command c set state = 'REJECTED', reason = 'already_final', delivery_pending = false
		from fx.payment p
		where c.id = $1 and p.id = c.subject_id::uuid and not p.workflow_start_pending and c.state = 'RECEIVED'`,
		commandID,
	)
	return err
}

// Deliver runs the executeCommand Update with the command id as its Update id, so a repeated
// delivery is answered by the first one.
func Deliver(ctx context.Context, c client.Client, paymentID, commandID string) (payment.Decision, error) {
	update, err := c.UpdateWorkflow(ctx, client.UpdateWorkflowOptions{
		WorkflowID:   payment.WorkflowID(paymentID),
		UpdateID:     commandID,
		UpdateName:   payment.UpdateExecuteCommand,
		Args:         []any{commandID},
		WaitForStage: client.WorkflowUpdateStageCompleted,
	})
	if err != nil {
		return payment.Decision{}, err
	}
	var decision payment.Decision
	return decision, update.Get(ctx, &decision)
}

func (d *Dispatcher) hints(ctx context.Context) (int, error) {
	pendings, err := d.Store.LeaseHints(ctx, laneSize, d.lease())
	if err != nil {
		return 0, err
	}
	handle(ctx, pendings, func(ctx context.Context, h store.PendingHint) {
		hint := payment.Hint{EventID: h.EventID, Reference: h.Reference, Status: h.Status, Sequence: h.Sequence}
		if err := d.signal(ctx, h.Provider, hint); err != nil {
			slog.WarnContext(ctx, "signal hint", "event", h.EventID, "error", err)
		}
	})
	return len(pendings), nil
}

func (d *Dispatcher) signal(ctx context.Context, provider string, h payment.Hint) error {
	status, err := d.route(ctx, h)
	if err != nil || status == "" {
		return err
	}
	_, err = d.Store.Pool().Exec(ctx, "update fx.webhook_inbox set status = $3, lease_until = null where provider = $1 and event_id = $2", provider, h.EventID, status)
	return err
}

// route signals the workflow the hint's payout belongs to and returns the status the inbox row
// ends with. A payment whose workflow is not started yet returns "", so the row stays pending
// until its lease expires.
func (d *Dispatcher) route(ctx context.Context, h payment.Hint) (string, error) {
	payout, err := uuid.Parse(h.Reference)
	if err != nil {
		return noTarget, nil
	}
	var (
		paymentID uuid.UUID
		pending   bool
	)
	err = d.Store.Pool().QueryRow(ctx, `
		select p.id, p.workflow_start_pending from fx.payout o join fx.payment p on p.id = o.payment_id where o.id = $1`,
		payout,
	).Scan(&paymentID, &pending)
	if errors.Is(err, pgx.ErrNoRows) {
		return noTarget, nil
	}
	if err != nil || pending {
		return "", err
	}
	callCtx, cancel := context.WithTimeout(ctx, d.callTimeout())
	defer cancel()
	err = d.Temporal.SignalWorkflow(callCtx, payment.WorkflowID(paymentID.String()), "", payment.SignalProviderHint, h)
	var notFound *serviceerror.NotFound
	if errors.As(err, &notFound) {
		return noTarget, nil
	}
	if err != nil {
		return "", err
	}
	return signalled, nil
}
