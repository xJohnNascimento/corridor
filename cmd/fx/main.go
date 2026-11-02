package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/xJohnNascimento/corridor/internal/config"
	"github.com/xJohnNascimento/corridor/internal/store"
	"github.com/xJohnNascimento/corridor/migrations"
)

const usage = "usage: fx api | worker | dispatch | migrate | cli | healthcheck"

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "fx:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New(usage)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	switch args[0] {
	case "api":
		return serveAPI(ctx)
	case "worker":
		return serveWorker(ctx)
	case "dispatch":
		return serveDispatch(ctx)
	case "migrate":
		return migrate(ctx, os.Getenv("FX_DATABASE_URL"))
	case "cli":
		return cli(ctx, args[1:])
	case "healthcheck":
		return healthcheck(ctx, envOr("FX_HEALTH_URL", "http://127.0.0.1:8080/healthz"))
	}
	return fmt.Errorf("unknown command %q; %s", args[0], usage)
}

type environment struct {
	cfg    *config.Config
	policy config.Policy
	pool   *pgxpool.Pool
	store  *store.Store
}

func load(ctx context.Context) (*environment, error) {
	cfg, err := config.Load(envOr("FX_CONFIG_FILE", "/etc/fx/config.yaml"))
	if err != nil {
		return nil, err
	}
	profile := envOr("FX_PROFILE", "production")
	if profile == "demo" && os.Getenv("FX_ENVIRONMENT") != "local" {
		return nil, errors.New("the demo profile runs only with FX_ENVIRONMENT=local")
	}
	policy, err := cfg.Policy(profile)
	if err != nil {
		return nil, err
	}
	databaseURL := os.Getenv("FX_DATABASE_URL")
	if databaseURL == "" {
		return nil, errors.New("FX_DATABASE_URL is required")
	}
	pool, err := openPool(ctx, databaseURL, os.Getenv("FX_APP_PASSWORD"))
	if err != nil {
		return nil, err
	}
	return &environment{cfg: cfg, policy: policy, pool: pool, store: store.New(pool)}, nil
}

func required(names ...string) (map[string]string, error) {
	values := make(map[string]string, len(names))
	var missing []string
	for _, name := range names {
		values[name] = os.Getenv(name)
		if values[name] == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("missing environment variables: %v", missing)
	}
	return values, nil
}

func migrate(ctx context.Context, databaseURL string) error {
	if databaseURL == "" {
		return errors.New("FX_DATABASE_URL is required")
	}
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	applied, err := migrations.Up(ctx, db)
	if err != nil {
		return err
	}
	slog.Info("migrations applied", "versions", applied)
	return migrations.SetRolePasswords(ctx, db, map[string]string{
		"fx_app":   os.Getenv("FX_APP_PASSWORD"),
		"fx_audit": os.Getenv("FX_AUDIT_PASSWORD"),
	})
}

// openPool takes the password apart from the URL, so a secret store injects only the password.
func openPool(ctx context.Context, databaseURL, password string) (*pgxpool.Pool, error) {
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, err
	}
	if password != "" {
		config.ConnConfig.Password = password
	}
	return pgxpool.NewWithConfig(ctx, config)
}

func healthcheck(ctx context.Context, url string) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("health check returned %s", resp.Status)
	}
	return nil
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
