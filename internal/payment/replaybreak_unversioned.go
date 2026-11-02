//go:build replaybreak

package payment

import (
	"time"

	"go.temporal.io/sdk/workflow"
)

func replayBreak(ctx workflow.Context) error {
	return workflow.Sleep(ctx, time.Millisecond)
}
