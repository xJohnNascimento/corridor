//go:build providertest

package providertest_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/xJohnNascimento/corridor/internal/provider"
	"github.com/xJohnNascimento/corridor/internal/provider/providertest"
)

type dittoSandbox struct {
	url, adminKey string
}

func (d dittoSandbox) Fund(ctx context.Context, reference, currency string, amount int64) error {
	body, _ := json.Marshal(map[string]string{"account": "certification", "reference": reference, "currency": currency, "amount": fmt.Sprint(amount)})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, d.url+"/admin/funding", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+d.adminKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("funding answered %s", resp.Status)
	}
	return nil
}

func TestDittoKeepsTheProviderContract(t *testing.T) {
	url := os.Getenv("DITTO_URL")
	if url == "" {
		url = "http://127.0.0.1:9002"
	}
	ditto := provider.NewDitto(&http.Client{Timeout: 10 * time.Second}, url, os.Getenv("DITTO_KEY_CERTIFICATION"))
	providertest.Run(t, ditto, dittoSandbox{url: url, adminKey: os.Getenv("DITTO_ADMIN_KEY")})
}
