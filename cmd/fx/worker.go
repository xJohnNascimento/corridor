package main

import (
	"context"
	"crypto/tls"
	"log/slog"
	"net/http"
	"os"
	"time"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/log"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"

	"github.com/xJohnNascimento/corridor/internal/dispatch"
	"github.com/xJohnNascimento/corridor/internal/failpoint"
	"github.com/xJohnNascimento/corridor/internal/ledger"
	"github.com/xJohnNascimento/corridor/internal/payment"
	"github.com/xJohnNascimento/corridor/internal/provider"
)

// providerActivitiesPerSecond shapes the provider queue below the provider's quota, leaving room
// for quotes; the 429 handling still matters because this is not an instantaneous cap.
const providerActivitiesPerSecond = 8

// temporalClient connects to Temporal. The API and the dispatcher use a lazy client, so they keep
// admitting and reading while Temporal is down; the worker waits until Temporal answers.
func temporalClient(ctx context.Context, lazy bool) (client.Client, error) {
	options := client.Options{
		HostPort:  envOr("TEMPORAL_ADDRESS", "127.0.0.1:7233"),
		Namespace: envOr("TEMPORAL_NAMESPACE", "default"),
		Logger:    log.NewStructuredLogger(slog.Default()),
	}
	if key := os.Getenv("TEMPORAL_API_KEY"); key != "" {
		options.Credentials = client.NewAPIKeyStaticCredentials(key)
		options.ConnectionOptions.TLS = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	if lazy {
		return client.NewLazyClient(options)
	}
	for {
		c, err := client.Dial(options)
		if err == nil {
			return c, nil
		}
		slog.WarnContext(ctx, "waiting for Temporal", "error", err)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// failpoints are armed only in the demo profile; the production profile never consults them.
func failpoints(env *environment) *failpoint.Points {
	if !isDemo() {
		return nil
	}
	return failpoint.New(env.pool)
}

func isDemo() bool {
	return envOr("FX_PROFILE", "production") == "demo"
}

func serveWorker(ctx context.Context) error {
	env, err := load(ctx)
	if err != nil {
		return err
	}
	defer env.pool.Close()
	secrets, err := required("FX_LEDGER_URL", "FX_LEDGER_SECRET", "FX_DITTO_URL", "FX_DITTO_KEY")
	if err != nil {
		return err
	}
	svc := ledger.NewService(&http.Client{Timeout: 5 * time.Second}, secrets["FX_LEDGER_URL"], secrets["FX_LEDGER_SECRET"])
	accounts, err := ledger.ResolveAccounts(ctx, svc, env.cfg.Ledger.ID, env.cfg.Ledger.ProviderOwner, env.cfg.Ledger.HouseOwner, env.cfg.Currencies)
	if err != nil {
		return err
	}
	points := failpoints(env)
	activities := &payment.Activities{
		Store:    env.store,
		Ledger:   ledger.New(svc, env.store, points, accounts),
		Provider: provider.NewDitto(&http.Client{Timeout: 10 * time.Second}, secrets["FX_DITTO_URL"], secrets["FX_DITTO_KEY"]),
		Points:   points,
	}
	c, err := temporalClient(ctx, false)
	if err != nil {
		return err
	}
	defer c.Close()

	payments := worker.New(c, payment.TaskQueue, worker.Options{WorkflowPanicPolicy: worker.BlockWorkflow})
	payments.RegisterWorkflowWithOptions(payment.PaymentWorkflow, workflow.RegisterOptions{Name: "PaymentWorkflow"})
	payments.RegisterActivity(activities)
	providerCalls := worker.New(c, payment.ProviderTaskQueue, worker.Options{TaskQueueActivitiesPerSecond: providerActivitiesPerSecond})
	providerCalls.RegisterActivity(activities)

	if err := payments.Start(); err != nil {
		return err
	}
	defer payments.Stop()
	if err := providerCalls.Start(); err != nil {
		return err
	}
	defer providerCalls.Stop()
	<-ctx.Done()
	return nil
}

func serveDispatch(ctx context.Context) error {
	env, err := load(ctx)
	if err != nil {
		return err
	}
	defer env.pool.Close()
	c, err := temporalClient(ctx, true)
	if err != nil {
		return err
	}
	defer c.Close()
	(&dispatch.Dispatcher{Store: env.store, Temporal: c}).Run(ctx, 500*time.Millisecond)
	return nil
}
