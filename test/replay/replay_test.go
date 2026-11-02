package replay_test

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"go.temporal.io/sdk/log"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"

	"github.com/xJohnNascimento/corridor/internal/payment"
)

// TestTheCorpusReplays replays every committed history against the current workflow code. A
// change to workflow commands that is not behind workflow.GetVersion fails here, before it can
// break a running payment.
func TestTheCorpusReplays(t *testing.T) {
	histories, err := filepath.Glob("*.json")
	if err != nil || len(histories) == 0 {
		t.Fatalf("no histories in the corpus: %v", err)
	}
	logger := log.NewStructuredLogger(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})))
	for _, history := range histories {
		t.Run(history, func(t *testing.T) {
			replayer := worker.NewWorkflowReplayer()
			replayer.RegisterWorkflowWithOptions(payment.PaymentWorkflow, workflow.RegisterOptions{Name: "PaymentWorkflow"})
			if err := replayer.ReplayWorkflowHistoryFromJSONFile(logger, history); err != nil {
				t.Fatal(err)
			}
		})
	}
}
