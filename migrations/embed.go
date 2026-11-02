package migrations

import (
	"context"
	"database/sql"
	"embed"
	"fmt"

	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"
)

//go:embed *.sql
var files embed.FS

func Up(ctx context.Context, db *sql.DB) ([]int64, error) {
	locker, err := lock.NewPostgresSessionLocker()
	if err != nil {
		return nil, err
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, db, files, goose.WithSessionLocker(locker))
	if err != nil {
		return nil, err
	}
	results, err := provider.Up(ctx)
	if err != nil {
		return nil, err
	}
	applied := make([]int64, 0, len(results))
	for _, result := range results {
		applied = append(applied, result.Source.Version)
	}
	return applied, nil
}

// SetRolePasswords sets the login password of each role whose password is not empty. It sends a
// SCRAM-SHA-256 verifier, so the plaintext never reaches the server's statement log. Migrations
// create the roles without passwords, so deployments that do not use trust auth call this on
// every run, which also rotates them.
func SetRolePasswords(ctx context.Context, db *sql.DB, passwords map[string]string) error {
	for role, password := range passwords {
		if password == "" {
			continue
		}
		verifier, err := scramVerifier(password)
		if err != nil {
			return fmt.Errorf("set the password of %s: %w", role, err)
		}
		var statement string
		err = db.QueryRowContext(ctx, "select format('alter role %I password %L', $1::text, $2::text)", role, verifier).Scan(&statement)
		if err != nil {
			return err
		}
		if _, err := db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("set the password of %s: %w", role, err)
		}
	}
	return nil
}
