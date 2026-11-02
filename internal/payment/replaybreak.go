//go:build !replaybreak && !replaybreak_versioned

package payment

import "go.temporal.io/sdk/workflow"

// replayBreak is where make replay-break inserts a new workflow command, once without and once
// with workflow.GetVersion, to show the replay gate catching an incompatible change.
func replayBreak(workflow.Context) error {
	return nil
}
