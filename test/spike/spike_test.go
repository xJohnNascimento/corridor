//go:build spike

package spike_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
)

func dial(t *testing.T) client.Client {
	t.Helper()
	address := os.Getenv("TEMPORAL_ADDRESS")
	if address == "" {
		address = "127.0.0.1:7243"
	}
	c, err := client.Dial(client.Options{HostPort: address})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}

func unique(t *testing.T) string {
	return fmt.Sprintf("%s-%d", strings.ReplaceAll(t.Name(), "/", "-"), time.Now().UnixNano())
}

func startWorker(t *testing.T, c client.Client, queue string, options worker.Options, register func(worker.Worker)) {
	t.Helper()
	w := worker.New(c, queue, options)
	register(w)
	if err := w.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(w.Stop)
}

func countEvents(t *testing.T, c client.Client, workflowID string, kind enumspb.EventType) int {
	t.Helper()
	iter := c.GetWorkflowHistory(t.Context(), workflowID, "", false, enumspb.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT)
	n := 0
	for iter.HasNext() {
		event, err := iter.Next()
		if err != nil {
			t.Fatal(err)
		}
		if event.GetEventType() == kind {
			n++
		}
	}
	return n
}

func admissionWorkflow(ctx workflow.Context, decideAfter time.Duration) (string, error) {
	status := "ADMITTED"
	err := workflow.SetUpdateHandler(ctx, "awaitConversion", func(ctx workflow.Context) (string, error) {
		_, err := workflow.AwaitWithTimeout(ctx, 2*time.Second, func() bool { return status != "ADMITTED" })
		return status, err
	})
	if err != nil {
		return "", err
	}
	if decideAfter > 0 {
		if err := workflow.Sleep(ctx, decideAfter); err != nil {
			return "", err
		}
		status = "CONVERTED"
	}
	if err := workflow.Sleep(ctx, 5*time.Second); err != nil {
		return "", err
	}
	return status, workflow.Await(ctx, func() bool { return workflow.AllHandlersFinished(ctx) })
}

func TestUpdateWithStartReturnsTheDecisionWithinItsBound(t *testing.T) {
	c := dial(t)
	queue := unique(t)
	startWorker(t, c, queue, worker.Options{}, func(w worker.Worker) { w.RegisterWorkflow(admissionWorkflow) })

	updateWithStart := func(id string, decideAfter time.Duration) (string, string, time.Duration) {
		op := c.NewWithStartWorkflowOperation(client.StartWorkflowOptions{
			ID:                       id,
			TaskQueue:                queue,
			WorkflowIDConflictPolicy: enumspb.WORKFLOW_ID_CONFLICT_POLICY_USE_EXISTING,
		}, admissionWorkflow, decideAfter)
		began := time.Now()
		handle, err := c.UpdateWithStartWorkflow(t.Context(), client.UpdateWithStartWorkflowOptions{
			StartWorkflowOperation: op,
			UpdateOptions: client.UpdateWorkflowOptions{
				UpdateID:     "admission/" + id,
				UpdateName:   "awaitConversion",
				WaitForStage: client.WorkflowUpdateStageCompleted,
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		var status string
		if err := handle.Get(t.Context(), &status); err != nil {
			t.Fatal(err)
		}
		return status, handle.RunID(), time.Since(began)
	}

	decided := unique(t) + "-decided"
	status, runID, took := updateWithStart(decided, time.Second)
	if status != "CONVERTED" || took >= 2*time.Second {
		t.Fatalf("decided: got %s after %v, want CONVERTED in under 2s", status, took)
	}
	again, againRunID, _ := updateWithStart(decided, time.Second)
	if again != "CONVERTED" || againRunID != runID {
		t.Fatalf("retry with the same update id: got %s on run %s, want CONVERTED on run %s", again, againRunID, runID)
	}

	undecided := unique(t) + "-undecided"
	status, _, took = updateWithStart(undecided, 0)
	if status != "ADMITTED" || took < 2*time.Second || took > 4*time.Second {
		t.Fatalf("undecided: got %s after %v, want ADMITTED after the 2s bound", status, took)
	}
}

func approvalWorkflow(ctx workflow.Context) error {
	approved := false
	err := workflow.SetUpdateHandlerWithOptions(ctx, "approve",
		func(ctx workflow.Context, approver string) (string, error) {
			approved = true
			return "approved by " + approver, nil
		},
		workflow.UpdateHandlerOptions{Validator: func(ctx workflow.Context, approver string) error {
			if approver == "" {
				return errors.New("approver required")
			}
			return nil
		}},
	)
	if err != nil {
		return err
	}
	return workflow.Await(ctx, func() bool { return approved && workflow.AllHandlersFinished(ctx) })
}

func TestValidatorRejectionLeavesNoHistory(t *testing.T) {
	c := dial(t)
	queue := unique(t)
	startWorker(t, c, queue, worker.Options{}, func(w worker.Worker) { w.RegisterWorkflow(approvalWorkflow) })
	id := unique(t)
	run, err := c.ExecuteWorkflow(t.Context(), client.StartWorkflowOptions{ID: id, TaskQueue: queue}, approvalWorkflow)
	if err != nil {
		t.Fatal(err)
	}

	update := func(approver string) (string, error) {
		handle, err := c.UpdateWorkflow(t.Context(), client.UpdateWorkflowOptions{
			WorkflowID:   id,
			UpdateName:   "approve",
			Args:         []any{approver},
			WaitForStage: client.WorkflowUpdateStageCompleted,
		})
		if err != nil {
			return "", err
		}
		var result string
		return result, handle.Get(t.Context(), &result)
	}

	if _, err := update(""); err == nil {
		t.Fatal("want the validator to reject an empty approver")
	}
	if n := countEvents(t, c, id, enumspb.EVENT_TYPE_WORKFLOW_EXECUTION_UPDATE_ACCEPTED); n != 0 {
		t.Fatalf("after a rejected update: %d accepted events, want 0", n)
	}
	if result, err := update("bob"); err != nil || result != "approved by bob" {
		t.Fatalf("valid update: got %q, %v", result, err)
	}
	if err := run.Get(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	if n := countEvents(t, c, id, enumspb.EVENT_TYPE_WORKFLOW_EXECUTION_UPDATE_ACCEPTED); n != 1 {
		t.Fatalf("after a valid update: %d accepted events, want 1", n)
	}
}

type flaky struct{ calls atomic.Int32 }

func (f *flaky) Flaky(ctx context.Context) (int32, error) {
	if f.calls.Add(1) < 5 {
		return 0, errors.New("still processing")
	}
	return activity.GetInfo(ctx).Attempt, nil
}

func retryWorkflow(ctx workflow.Context) (int32, error) {
	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 5 * time.Second,
		RetryPolicy:         &temporal.RetryPolicy{InitialInterval: 50 * time.Millisecond, BackoffCoefficient: 1},
	})
	var attempt int32
	err := workflow.ExecuteActivity(ctx, "Flaky").Get(ctx, &attempt)
	return attempt, err
}

func TestActivityRetriesAddNoHistory(t *testing.T) {
	c := dial(t)
	queue := unique(t)
	acts := &flaky{}
	startWorker(t, c, queue, worker.Options{}, func(w worker.Worker) {
		w.RegisterWorkflow(retryWorkflow)
		w.RegisterActivityWithOptions(acts.Flaky, activity.RegisterOptions{Name: "Flaky"})
	})
	id := unique(t)
	run, err := c.ExecuteWorkflow(t.Context(), client.StartWorkflowOptions{ID: id, TaskQueue: queue}, retryWorkflow)
	if err != nil {
		t.Fatal(err)
	}
	var attempt int32
	if err := run.Get(t.Context(), &attempt); err != nil {
		t.Fatal(err)
	}
	if attempt != 5 {
		t.Fatalf("succeeded on attempt %d, want 5", attempt)
	}
	for kind, want := range map[enumspb.EventType]int{
		enumspb.EVENT_TYPE_ACTIVITY_TASK_SCHEDULED: 1,
		enumspb.EVENT_TYPE_ACTIVITY_TASK_STARTED:   1,
		enumspb.EVENT_TYPE_ACTIVITY_TASK_COMPLETED: 1,
		enumspb.EVENT_TYPE_ACTIVITY_TASK_FAILED:    0,
	} {
		if got := countEvents(t, c, id, kind); got != want {
			t.Errorf("%v events = %d, want %d", kind, got, want)
		}
	}
}

func longWorkflow(ctx workflow.Context) (int, error) {
	for range 500 {
		info := workflow.GetInfo(ctx)
		if info.GetContinueAsNewSuggested() {
			return info.GetCurrentHistoryLength(), nil
		}
		if err := workflow.Sleep(ctx, time.Millisecond); err != nil {
			return 0, err
		}
	}
	return 0, errors.New("continue-as-new was never suggested")
}

func TestContinueAsNewIsSuggestedAtTheConfiguredLimit(t *testing.T) {
	c := dial(t)
	queue := unique(t)
	startWorker(t, c, queue, worker.Options{}, func(w worker.Worker) { w.RegisterWorkflow(longWorkflow) })
	run, err := c.ExecuteWorkflow(t.Context(), client.StartWorkflowOptions{ID: unique(t), TaskQueue: queue}, longWorkflow)
	if err != nil {
		t.Fatal(err)
	}
	var length int
	if err := run.Get(t.Context(), &length); err != nil {
		t.Fatal(err)
	}
	if length < 50 || length > 100 {
		t.Fatalf("suggested at %d events, want just past the configured 50", length)
	}
}

func stamp(context.Context) (time.Time, error) {
	return time.Now(), nil
}

func fanOutWorkflow(ctx workflow.Context, n int) ([]time.Time, error) {
	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{StartToCloseTimeout: 5 * time.Second})
	futures := make([]workflow.Future, n)
	for i := range futures {
		futures[i] = workflow.ExecuteActivity(ctx, "Stamp")
	}
	stamps := make([]time.Time, n)
	for i, future := range futures {
		if err := future.Get(ctx, &stamps[i]); err != nil {
			return nil, err
		}
	}
	return stamps, nil
}

func TestTaskQueueActivityRateIsEnforcedAcrossWorkers(t *testing.T) {
	c := dial(t)
	queue := unique(t)
	register := func(w worker.Worker) {
		w.RegisterWorkflow(fanOutWorkflow)
		w.RegisterActivityWithOptions(stamp, activity.RegisterOptions{Name: "Stamp"})
	}
	startWorker(t, c, queue, worker.Options{TaskQueueActivitiesPerSecond: 2}, register)
	startWorker(t, c, queue, worker.Options{TaskQueueActivitiesPerSecond: 2}, register)

	const n = 12
	run, err := c.ExecuteWorkflow(t.Context(), client.StartWorkflowOptions{ID: unique(t), TaskQueue: queue}, fanOutWorkflow, n)
	if err != nil {
		t.Fatal(err)
	}
	var stamps []time.Time
	if err := run.Get(t.Context(), &stamps); err != nil {
		t.Fatal(err)
	}
	slices.SortFunc(stamps, func(a, b time.Time) int { return a.Compare(b) })
	rate := float64(n-1) / stamps[n-1].Sub(stamps[0]).Seconds()
	t.Logf("%d activities across two workers ran at %.2f per second", n, rate)
	if rate > 3 {
		t.Fatalf("rate %.2f/s: the limit was applied per worker (about 4/s), not per task queue (2/s)", rate)
	}
}

func finishingWorkflow(ctx workflow.Context) error {
	err := workflow.SetUpdateHandler(ctx, "slow", func(ctx workflow.Context) (string, error) {
		if err := workflow.Sleep(ctx, 2*time.Second); err != nil {
			return "", err
		}
		return "done", nil
	})
	if err != nil {
		return err
	}
	var finish bool
	workflow.GetSignalChannel(ctx, "finish").Receive(ctx, &finish)
	return workflow.Await(ctx, func() bool { return workflow.AllHandlersFinished(ctx) })
}

func TestAllHandlersFinishedLetsAnInFlightUpdateComplete(t *testing.T) {
	c := dial(t)
	queue := unique(t)
	startWorker(t, c, queue, worker.Options{}, func(w worker.Worker) { w.RegisterWorkflow(finishingWorkflow) })
	id := unique(t)
	run, err := c.ExecuteWorkflow(t.Context(), client.StartWorkflowOptions{ID: id, TaskQueue: queue}, finishingWorkflow)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := c.UpdateWorkflow(t.Context(), client.UpdateWorkflowOptions{
		WorkflowID:   id,
		UpdateName:   "slow",
		WaitForStage: client.WorkflowUpdateStageAccepted,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.SignalWorkflow(t.Context(), id, "", "finish", true); err != nil {
		t.Fatal(err)
	}
	var result string
	if err := handle.Get(t.Context(), &result); err != nil || result != "done" {
		t.Fatalf("in-flight update: got %q, %v; want done", result, err)
	}
	if err := run.Get(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
}

type cleanup struct{ calls atomic.Int32 }

func (c *cleanup) MarkRecoveryRequired(context.Context) error {
	c.calls.Add(1)
	return nil
}

func cancellableWorkflow(ctx workflow.Context) error {
	err := workflow.Sleep(ctx, time.Hour)
	if !temporal.IsCanceledError(err) {
		return err
	}
	cleanupCtx, _ := workflow.NewDisconnectedContext(ctx)
	cleanupCtx = workflow.WithActivityOptions(cleanupCtx, workflow.ActivityOptions{StartToCloseTimeout: 5 * time.Second})
	if cleanupErr := workflow.ExecuteActivity(cleanupCtx, "MarkRecoveryRequired").Get(cleanupCtx, nil); cleanupErr != nil {
		return cleanupErr
	}
	return err
}

func TestCancellationRunsTheDisconnectedCleanup(t *testing.T) {
	c := dial(t)
	queue := unique(t)
	acts := &cleanup{}
	startWorker(t, c, queue, worker.Options{}, func(w worker.Worker) {
		w.RegisterWorkflow(cancellableWorkflow)
		w.RegisterActivityWithOptions(acts.MarkRecoveryRequired, activity.RegisterOptions{Name: "MarkRecoveryRequired"})
	})
	id := unique(t)
	run, err := c.ExecuteWorkflow(t.Context(), client.StartWorkflowOptions{ID: id, TaskQueue: queue}, cancellableWorkflow)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	if err := c.CancelWorkflow(t.Context(), id, ""); err != nil {
		t.Fatal(err)
	}
	if err := run.Get(t.Context(), nil); !temporal.IsCanceledError(err) {
		t.Fatalf("workflow result: %v, want a cancellation", err)
	}
	if n := acts.calls.Load(); n != 1 {
		t.Fatalf("cleanup ran %d times, want 1", n)
	}
}

func quickWorkflow(workflow.Context) error {
	return nil
}

func waitingWorkflow(ctx workflow.Context) error {
	return workflow.Sleep(ctx, time.Hour)
}

func TestWorkflowIDReusePolicies(t *testing.T) {
	c := dial(t)
	queue := unique(t)
	startWorker(t, c, queue, worker.Options{}, func(w worker.Worker) {
		w.RegisterWorkflow(quickWorkflow)
		w.RegisterWorkflow(waitingWorkflow)
	})
	ctx := t.Context()
	start := func(id string, reuse enumspb.WorkflowIdReusePolicy, conflict enumspb.WorkflowIdConflictPolicy, fn any) (client.WorkflowRun, error) {
		return c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
			ID:                                       id,
			TaskQueue:                                queue,
			WorkflowIDReusePolicy:                    reuse,
			WorkflowIDConflictPolicy:                 conflict,
			WorkflowExecutionErrorWhenAlreadyStarted: true,
		}, fn)
	}
	alreadyStarted := func(err error) bool {
		var target *serviceerror.WorkflowExecutionAlreadyStarted
		return errors.As(err, &target)
	}

	completed := unique(t) + "-completed"
	run, err := start(completed, enumspb.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE, enumspb.WORKFLOW_ID_CONFLICT_POLICY_FAIL, quickWorkflow)
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Get(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := start(completed, enumspb.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE, enumspb.WORKFLOW_ID_CONFLICT_POLICY_FAIL, quickWorkflow); !alreadyStarted(err) {
		t.Errorf("REJECT_DUPLICATE after completion: %v, want already started", err)
	}
	if _, err := start(completed, enumspb.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE_FAILED_ONLY, enumspb.WORKFLOW_ID_CONFLICT_POLICY_FAIL, quickWorkflow); !alreadyStarted(err) {
		t.Errorf("ALLOW_DUPLICATE_FAILED_ONLY after completion: %v, want already started", err)
	}

	running := unique(t) + "-running"
	first, err := start(running, enumspb.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE, enumspb.WORKFLOW_ID_CONFLICT_POLICY_FAIL, waitingWorkflow)
	if err != nil {
		t.Fatal(err)
	}
	existing, err := start(running, enumspb.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE, enumspb.WORKFLOW_ID_CONFLICT_POLICY_USE_EXISTING, waitingWorkflow)
	if err != nil || existing.GetRunID() != first.GetRunID() {
		t.Errorf("USE_EXISTING on a running workflow: run %v, %v; want the same run %s", existing, err, first.GetRunID())
	}
	if err := c.TerminateWorkflow(ctx, running, "", "spike"); err != nil {
		t.Fatal(err)
	}
	resumed, err := start(running, enumspb.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE_FAILED_ONLY, enumspb.WORKFLOW_ID_CONFLICT_POLICY_FAIL, waitingWorkflow)
	if err != nil || resumed.GetRunID() == first.GetRunID() {
		t.Fatalf("ALLOW_DUPLICATE_FAILED_ONLY after termination: %v; want a new run", err)
	}
	if err := c.TerminateWorkflow(ctx, running, "", "spike cleanup"); err != nil {
		t.Fatal(err)
	}
}
