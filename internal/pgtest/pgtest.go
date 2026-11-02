package pgtest

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/xJohnNascimento/corridor/migrations"
)

const (
	image    = "postgres:17-alpine"
	template = "fx_template"
)

type Database struct {
	Name  string
	Owner *pgxpool.Pool
	App   *pgxpool.Pool
	Audit *pgxpool.Pool
}

var (
	hostPort string
	admin    *pgxpool.Pool
	cloning  sync.Mutex
	sequence atomic.Int64
)

func Main(m *testing.M) int {
	ctx := context.Background()
	container, err := postgres.Run(ctx, image,
		postgres.WithDatabase(template),
		postgres.WithUsername("postgres"),
		postgres.WithPassword("postgres"),
		testcontainers.WithEnv(map[string]string{"POSTGRES_HOST_AUTH_METHOD": "trust"}),
		testcontainers.WithCmd("postgres", "-c", "fsync=off", "-c", "max_connections=400"),
		postgres.BasicWaitStrategies(),
	)
	defer func() { _ = testcontainers.TerminateContainer(container) }()
	if err != nil {
		return fail(err)
	}
	connection, err := container.ConnectionString(ctx)
	if err != nil {
		return fail(err)
	}
	parsed, err := url.Parse(connection)
	if err != nil {
		return fail(err)
	}
	hostPort = parsed.Host

	if err := migrate(ctx); err != nil {
		return fail(err)
	}
	admin, err = pgxpool.New(ctx, URL("postgres", "postgres"))
	if err != nil {
		return fail(err)
	}
	defer admin.Close()
	return m.Run()
}

func URL(role, database string) string {
	return fmt.Sprintf("postgres://%s@%s/%s?sslmode=disable", role, hostPort, database)
}

func NewDatabase(t testing.TB) Database {
	t.Helper()
	ctx := context.Background()
	name := fmt.Sprintf("fx_test_%d", sequence.Add(1))

	cloning.Lock()
	_, err := admin.Exec(ctx, "create database "+name+" template "+template)
	cloning.Unlock()
	if err != nil {
		t.Fatalf("create database %s: %v", name, err)
	}
	db := Database{
		Name:  name,
		Owner: open(t, "postgres", name),
		App:   open(t, "fx_app", name),
		Audit: open(t, "fx_audit", name),
	}
	t.Cleanup(func() {
		db.Owner.Close()
		db.App.Close()
		db.Audit.Close()
		if _, err := admin.Exec(ctx, "drop database "+name+" with (force)"); err != nil {
			t.Errorf("drop database %s: %v", name, err)
		}
	})
	return db
}

func open(t testing.TB, role, database string) *pgxpool.Pool {
	t.Helper()
	config, err := pgxpool.ParseConfig(URL(role, database))
	if err != nil {
		t.Fatalf("connect as %s: %v", role, err)
	}
	config.MaxConns = 32
	pool, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		t.Fatalf("connect as %s: %v", role, err)
	}
	return pool
}

func migrate(ctx context.Context) error {
	db, err := sql.Open("pgx", URL("postgres", template))
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	_, err = migrations.Up(ctx, db)
	return err
}

func fail(err error) int {
	fmt.Fprintln(os.Stderr, "pgtest:", err)
	return 1
}
