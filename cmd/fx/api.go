package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/xJohnNascimento/corridor/internal/api"
	"github.com/xJohnNascimento/corridor/internal/provider"
	"github.com/xJohnNascimento/corridor/internal/store"
)

type operator struct {
	id  string
	key string
}

func operatorKeyVariable(id string) string {
	return "FX_OPERATOR_KEY_" + strings.ToUpper(id)
}

// operatorsFromEnv reads the operator ids from FX_OPERATORS and each key from FX_OPERATOR_KEY_<ID>.
func operatorsFromEnv() ([]operator, error) {
	var ids, variables []string
	for _, id := range strings.Split(os.Getenv("FX_OPERATORS"), ",") {
		id = strings.ToLower(strings.TrimSpace(id))
		if id == "" || slices.Contains(ids, id) {
			continue
		}
		ids, variables = append(ids, id), append(variables, operatorKeyVariable(id))
	}
	if len(ids) == 0 {
		return nil, errors.New("FX_OPERATORS is required")
	}
	keys, err := required(variables...)
	if err != nil {
		return nil, err
	}
	operators := make([]operator, len(ids))
	for i, id := range ids {
		operators[i] = operator{id: id, key: keys[variables[i]]}
	}
	return operators, nil
}

func serveAPI(ctx context.Context) error {
	env, err := load(ctx)
	if err != nil {
		return err
	}
	defer env.pool.Close()
	secrets, err := required("FX_DITTO_URL", "FX_DITTO_KEY", "FX_AUDITOR_KEY", "FX_WEBHOOK_SECRET", "FX_AUDIT_DATABASE_URL")
	if err != nil {
		return err
	}
	operators, err := operatorsFromEnv()
	if err != nil {
		return err
	}
	webhookSecrets, err := api.ParseWebhookSecrets(secrets["FX_WEBHOOK_SECRET"])
	if err != nil {
		return err
	}
	auditPool, err := openPool(ctx, secrets["FX_AUDIT_DATABASE_URL"], os.Getenv("FX_AUDIT_PASSWORD"))
	if err != nil {
		return err
	}
	defer auditPool.Close()
	for _, o := range operators {
		if err := env.store.UpsertOperator(ctx, o.id, o.id, store.KeyHash(o.key)); err != nil {
			return err
		}
	}

	temporal, err := temporalClient(ctx, true)
	if err != nil {
		return err
	}
	defer temporal.Close()
	server := api.New(api.Deps{
		Temporal:       temporal,
		Points:         failpoints(env),
		Store:          env.store,
		Config:         env.cfg,
		Policy:         env.policy,
		Provider:       provider.NewDitto(&http.Client{Timeout: 10 * time.Second}, secrets["FX_DITTO_URL"], secrets["FX_DITTO_KEY"]),
		AuditorKeyHash: store.KeyHash(secrets["FX_AUDITOR_KEY"]),
		WebhookSecrets: webhookSecrets,
		AuditStore:     store.New(auditPool),
		DatasetID:      envOr("FX_DATASET_ID", "local"),
	})
	servers := []*http.Server{
		newServer(envOr("FX_CUSTOMER_ADDR", ":8080"), server.CustomerHandler()),
		newServer(envOr("FX_INTERNAL_ADDR", ":8081"), server.InternalHandler()),
	}
	group, ctx := errgroup.WithContext(ctx)
	for _, srv := range servers {
		group.Go(func() error {
			slog.Info("fx api listening", "addr", srv.Addr)
			if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
				return err
			}
			return nil
		})
	}
	group.Go(func() error {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		for _, srv := range servers {
			_ = srv.Shutdown(shutdownCtx)
		}
		return nil
	})
	return group.Wait()
}

func newServer(addr string, handler http.Handler) *http.Server {
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		Protocols:         protocols,
		ReadHeaderTimeout: 5 * time.Second,
	}
}
