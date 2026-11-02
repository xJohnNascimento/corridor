package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"time"

	"go.temporal.io/sdk/client"

	"github.com/xJohnNascimento/corridor/verification"
)

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// main exits 0 on PASS, 1 on FAIL and 2 on INCOMPLETE. The work runs in run so that its deferred
// cleanup finishes before os.Exit.
func main() {
	os.Exit(run())
}

func run() int {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	temporal, err := client.NewLazyClient(client.Options{HostPort: envOr("TEMPORAL_ADDRESS", "127.0.0.1:7233")})
	if err != nil {
		fmt.Fprintln(os.Stderr, "verify:", err)
		return 2
	}
	defer temporal.Close()
	d, err := verification.Collect(ctx, verification.Sources{
		HTTP: &http.Client{Timeout: time.Minute}, LedgerURL: envOr("LEDGER_URL", "http://127.0.0.1:9090"),
		LedgerSecret: os.Getenv("LEDGER_SECRET_CORRIDOR_AUDIT"), LedgerID: "corridor",
		CorridorURL: envOr("FX_INTERNAL_URL", "http://127.0.0.1:8081"), AuditorKey: os.Getenv("FX_AUDITOR_KEY"),
		DittoURL: envOr("DITTO_URL", "http://127.0.0.1:9002"), DittoKey: os.Getenv("DITTO_KEY_CORRIDOR"), Temporal: temporal,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "verify:", err)
		return 2
	}
	report := verification.Verify(d)
	report.Print(os.Stdout)
	switch report.Result {
	case verification.Fail:
		return 1
	case verification.Incomplete:
		return 2
	}
	return 0
}
