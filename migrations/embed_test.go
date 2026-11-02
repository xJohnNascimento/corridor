package migrations_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/sha256"
	"encoding/base64"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/stdlib"

	"github.com/xJohnNascimento/corridor/internal/pgtest"
	"github.com/xJohnNascimento/corridor/migrations"
)

func TestMain(m *testing.M) {
	os.Exit(pgtest.Main(m))
}

func TestSetRolePasswordsSetsOnlyTheGivenPasswords(t *testing.T) {
	owner := pgtest.NewDatabase(t).Owner
	db := stdlib.OpenDBFromPool(owner)
	ctx := context.Background()
	t.Cleanup(func() {
		_, _ = owner.Exec(ctx, "alter role fx_app password null")
		_ = db.Close()
	})

	const password = "it's a secret"
	err := migrations.SetRolePasswords(ctx, db, map[string]string{"fx_app": password, "fx_audit": ""})
	if err != nil {
		t.Fatal(err)
	}
	var app, audit *string
	err = owner.QueryRow(ctx,
		`select max(rolpassword) filter (where rolname = 'fx_app'),
		        max(rolpassword) filter (where rolname = 'fx_audit')
		 from pg_authid`,
	).Scan(&app, &audit)
	if err != nil || app == nil || audit != nil {
		t.Fatalf("app verifier %v, audit verifier %v, err %v; want only the app's", app, audit, err)
	}
	if strings.Contains(*app, password) {
		t.Fatal("the stored value contains the plaintext password")
	}
	requireVerifies(t, *app, password)
	if err := migrations.SetRolePasswords(ctx, db, map[string]string{"fx_app": password}); err != nil {
		t.Fatal(err)
	}
	var rotated string
	if err := owner.QueryRow(ctx, "select rolpassword from pg_authid where rolname = 'fx_app'").Scan(&rotated); err != nil {
		t.Fatal(err)
	}
	if rotated == *app {
		t.Fatal("a second call must draw a fresh salt")
	}
}

func TestSetRolePasswordsRefusesWhatSASLprepWouldChange(t *testing.T) {
	db := stdlib.OpenDBFromPool(pgtest.NewDatabase(t).Owner)
	t.Cleanup(func() { _ = db.Close() })
	if err := migrations.SetRolePasswords(context.Background(), db, map[string]string{"fx_app": "pässword"}); err == nil {
		t.Fatal("a non-ASCII password was accepted")
	}
}

// requireVerifies recomputes the SCRAM keys from the password and the stored salt and iteration
// count, the same derivation a login performs, since the test container uses trust auth.
func requireVerifies(t *testing.T, verifier, password string) {
	t.Helper()
	scheme, rest, _ := strings.Cut(verifier, "$")
	params, keys, _ := strings.Cut(rest, "$")
	iterations, salt, _ := strings.Cut(params, ":")
	storedKey, serverKey, _ := strings.Cut(keys, ":")
	if scheme != "SCRAM-SHA-256" || iterations != "4096" {
		t.Fatalf("verifier %q: want SCRAM-SHA-256 with 4096 iterations", verifier)
	}
	rounds, _ := strconv.Atoi(iterations)
	rawSalt, err := base64.StdEncoding.DecodeString(salt)
	if err != nil || len(rawSalt) != 16 {
		t.Fatalf("salt %q: %d bytes, err %v; want 16", salt, len(rawSalt), err)
	}
	salted, err := pbkdf2.Key(sha256.New, password, rawSalt, rounds, sha256.Size)
	if err != nil {
		t.Fatal(err)
	}
	mac := func(message string) []byte {
		h := hmac.New(sha256.New, salted)
		h.Write([]byte(message))
		return h.Sum(nil)
	}
	clientKey := mac("Client Key")
	wantStored := sha256.Sum256(clientKey)
	gotStored, _ := base64.StdEncoding.DecodeString(storedKey)
	gotServer, _ := base64.StdEncoding.DecodeString(serverKey)
	if !bytes.Equal(gotStored, wantStored[:]) {
		t.Fatal("the stored key does not match the password")
	}
	if !bytes.Equal(gotServer, mac("Server Key")) {
		t.Fatal("the server key does not match the password")
	}
}
