//go:build replaybreak_versioned

package payment

import (
	"time"

	"go.temporal.io/sdk/workflow"
)

func replayBreak(ctx workflow.Context) error {
	if workflow.GetVersion(ctx, "replay-break", workflow.DefaultVersion, 1) == workflow.DefaultVersion {
		return nil
	}
	return workflow.Sleep(ctx, time.Millisecond)
}
