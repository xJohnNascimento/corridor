package provider_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/xJohnNascimento/corridor/internal/provider"
)

func TestTheDittoAdapterMapsStatusesToTheErrorsCallersBranchOn(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		status     int
		retryAfter string
		check      func(error) bool
	}{
		"404 is not visible":                     {http.StatusNotFound, "", func(err error) bool { return errors.Is(err, provider.ErrNotVisible) }},
		"409 is a conflicting reference":         {http.StatusConflict, "", func(err error) bool { return errors.Is(err, provider.ErrConflict) }},
		"429 carries Retry-After":                {http.StatusTooManyRequests, "7", rateLimited(7 * time.Second)},
		"429 without Retry-After waits a second": {http.StatusTooManyRequests, "", rateLimited(time.Second)},
		"429 with an HTTP date waits a second":   {http.StatusTooManyRequests, "Wed, 21 Oct 2026 07:28:00 GMT", rateLimited(time.Second)},
		"429 with zero waits a second":           {http.StatusTooManyRequests, "0", rateLimited(time.Second)},
		"500 is a plain error": {http.StatusInternalServerError, "", func(err error) bool {
			var limited *provider.RateLimitedError
			return err != nil && !errors.Is(err, provider.ErrNotVisible) && !errors.Is(err, provider.ErrConflict) && !errors.As(err, &limited)
		}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if c.retryAfter != "" {
					w.Header().Set("Retry-After", c.retryAfter)
				}
				w.WriteHeader(c.status)
			}))
			defer server.Close()
			ditto := provider.NewDitto(server.Client(), server.URL, "key")
			if _, err := ditto.Trade(context.Background(), "ref-1"); !c.check(err) {
				t.Fatalf("got %v", err)
			}
		})
	}
}

func rateLimited(want time.Duration) func(error) bool {
	return func(err error) bool {
		var limited *provider.RateLimitedError
		return errors.As(err, &limited) && limited.RetryAfter == want
	}
}

func TestTheDittoAdapterAuthenticatesAndDecodesASuccess(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" || r.URL.EscapedPath() != "/v1/trades/by-reference/ref%2F1" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`{"reference":"ref/1","status":"FILLED","buy_amount":"731325"}`))
	}))
	defer server.Close()
	trade, err := provider.NewDitto(server.Client(), server.URL, "secret").Trade(context.Background(), "ref/1")
	if err != nil || trade.Status != provider.TradeFilled || trade.BuyAmount != "731325" {
		t.Fatalf("%+v, %v", trade, err)
	}
}
