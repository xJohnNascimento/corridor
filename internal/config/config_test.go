package config_test

import (
	"strings"
	"testing"

	"github.com/xJohnNascimento/corridor/internal/config"
)

const valid = `
currencies: [CAD, USD]
corridors: [{sell: CAD, buy: USD}]
limits: {min_sell: 10000, max_sell: 25000000}
pricing: {fee_bps: 50, spread_bps: 5, fill_tolerance_bps: 10}
quotes: {ttl: 30s, booking_margin: 15s, per_second: 1, burst: 5}
ledger: {id: corridor, provider_owner: ditto, house_owner: house}
profiles:
  demo: {cancel_grace: 20s, fast_path_wait: 2s, trade_safety: 5s, poll_initial: 2s, poll_max: 10s, poll_horizon: 2m, liquidity_retry: 10s}
`

func TestConfigurationIsValidatedWhenParsed(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		old, new string
		want     string
	}{
		"valid":                       {"", "", ""},
		"a lowercase currency":        {"[CAD, USD]", "[cad, USD]", "three capital letters"},
		"a corridor to itself":        {"{sell: CAD, buy: USD}", "{sell: CAD, buy: CAD}", "unknown or repeated"},
		"a corridor to a stranger":    {"{sell: CAD, buy: USD}", "{sell: CAD, buy: EUR}", "unknown or repeated"},
		"a minimum above the maximum": {"min_sell: 10000", "min_sell: 30000000", "min_sell"},
		"no minimum":                  {"min_sell: 10000", "min_sell: 0", "min_sell"},
		"a negative fill tolerance":   {"fill_tolerance_bps: 10", "fill_tolerance_bps: -1", "fill_tolerance_bps"},
		"a fill tolerance over 100%":  {"fill_tolerance_bps: 10", "fill_tolerance_bps: 10001", "fill_tolerance_bps"},
		"no quote ttl":                {"ttl: 30s", "ttl: 0s", "quotes need"},
		"no quote rate":               {"per_second: 1", "per_second: 0", "quotes need"},
		"no quote burst":              {"burst: 5", "burst: 0", "quotes need"},
		"no ledger id":                {"id: corridor", "id: ''", "ledger needs"},
		"no cancel grace":             {"cancel_grace: 20s", "cancel_grace: 0s", "profile demo"},
		"a poll max below initial":    {"poll_max: 10s", "poll_max: 1s", "profile demo"},
		"an unknown field":            {"burst: 5", "burst: 5, history_limit: 10", "history_limit"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cfg, err := config.Parse([]byte(strings.Replace(valid, c.old, c.new, 1)))
			if c.want == "" {
				if err != nil || cfg == nil {
					t.Fatalf("a valid configuration was refused: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("got %v, want an error mentioning %q", err, c.want)
			}
		})
	}
}

func TestAPolicyCarriesTheConfigurationVersionAndFillTolerance(t *testing.T) {
	t.Parallel()
	cfg, err := config.Parse([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	policy, err := cfg.Policy("demo")
	if err != nil || policy.FillToleranceBps != 10 || policy.Version != cfg.Version+"/demo" {
		t.Fatalf("%+v, %v", policy, err)
	}
	if _, err := cfg.Policy("production"); err == nil {
		t.Fatal("an unconfigured profile was returned")
	}
}
