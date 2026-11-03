package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"slices"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/xJohnNascimento/corridor/internal/money"
	"github.com/xJohnNascimento/corridor/internal/pricing"
)

type Pair struct {
	Sell string `yaml:"sell"`
	Buy  string `yaml:"buy"`
}

// Policy is copied into every payment at admission, so a running workflow never reads
// configuration and a change never alters an accepted payment.
type Policy struct {
	Version          string        `json:"version"`
	CancelGrace      time.Duration `json:"cancel_grace" yaml:"cancel_grace"`
	FastPathWait     time.Duration `json:"fast_path_wait" yaml:"fast_path_wait"`
	TradeSafety      time.Duration `json:"trade_safety" yaml:"trade_safety"`
	PollInitial      time.Duration `json:"poll_initial" yaml:"poll_initial"`
	PollMax          time.Duration `json:"poll_max" yaml:"poll_max"`
	PollHorizon      time.Duration `json:"poll_horizon" yaml:"poll_horizon"`
	LiquidityRetry   time.Duration `json:"liquidity_retry" yaml:"liquidity_retry"`
	FillToleranceBps int64         `json:"fill_tolerance_bps" yaml:"-"`
}

type Config struct {
	Currencies []string `yaml:"currencies"`
	Corridors  []Pair   `yaml:"corridors"`
	Limits     struct {
		MinSell int64 `yaml:"min_sell"`
		MaxSell int64 `yaml:"max_sell"`
	} `yaml:"limits"`
	Pricing struct {
		FeeBps           int64 `yaml:"fee_bps"`
		SpreadBps        int64 `yaml:"spread_bps"`
		FillToleranceBps int64 `yaml:"fill_tolerance_bps"`
	} `yaml:"pricing"`
	Quotes struct {
		TTL           time.Duration `yaml:"ttl"`
		BookingMargin time.Duration `yaml:"booking_margin"`
		PerSecond     float64       `yaml:"per_second"`
		Burst         int           `yaml:"burst"`
	} `yaml:"quotes"`
	Ledger struct {
		ID            string `yaml:"id"`
		ProviderOwner string `yaml:"provider_owner"`
		HouseOwner    string `yaml:"house_owner"`
	} `yaml:"ledger"`
	Profiles map[string]Policy `yaml:"profiles"`

	Version string `yaml:"-"`
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path) //nolint:gosec // the path is the operator's own FX_CONFIG_FILE
	if err != nil {
		return nil, err
	}
	return Parse(data)
}

func Parse(data []byte) (*Config, error) {
	var c Config
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&c); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	sum := sha256.Sum256(data)
	c.Version = hex.EncodeToString(sum[:8])
	return &c, c.validate()
}

func (c *Config) validate() error {
	for _, currency := range c.Currencies {
		if _, err := money.ParseCurrency(currency); err != nil {
			return err
		}
	}
	for _, p := range c.Corridors {
		if p.Sell == p.Buy || !slices.Contains(c.Currencies, p.Sell) || !slices.Contains(c.Currencies, p.Buy) {
			return fmt.Errorf("corridor %s→%s uses an unknown or repeated currency", p.Sell, p.Buy)
		}
	}
	if c.Limits.MinSell <= 0 || c.Limits.MaxSell < c.Limits.MinSell {
		return errors.New("limits need 0 < min_sell ≤ max_sell")
	}
	if c.Pricing.FillToleranceBps < 0 || c.Pricing.FillToleranceBps > 10_000 {
		return errors.New("pricing needs 0 ≤ fill_tolerance_bps ≤ 10000")
	}
	if c.Quotes.TTL <= 0 || c.Quotes.PerSecond <= 0 || c.Quotes.Burst <= 0 {
		return errors.New("quotes need a positive ttl, per_second and burst")
	}
	if c.Ledger.ID == "" || c.Ledger.ProviderOwner == "" || c.Ledger.HouseOwner == "" {
		return errors.New("ledger needs id, provider_owner and house_owner")
	}
	for name, p := range c.Profiles {
		if p.CancelGrace <= 0 || p.FastPathWait <= 0 || p.PollInitial <= 0 || p.PollMax < p.PollInitial || p.PollHorizon <= 0 || p.LiquidityRetry <= 0 {
			return fmt.Errorf("profile %s has a missing or inconsistent duration", name)
		}
	}
	return nil
}

func (c *Config) Enabled(sell, buy string) bool {
	return slices.Contains(c.Corridors, Pair{Sell: sell, Buy: buy})
}

func (c *Config) Schedule() pricing.Schedule {
	return pricing.Schedule{FeeBps: c.Pricing.FeeBps, SpreadBps: c.Pricing.SpreadBps}
}

// Policy returns the named profile, stamped with this configuration's version and its fill tolerance.
func (c *Config) Policy(profile string) (Policy, error) {
	p, ok := c.Profiles[profile]
	if !ok {
		return Policy{}, fmt.Errorf("profile %q is not configured", profile)
	}
	p.Version = c.Version + "/" + profile
	p.FillToleranceBps = c.Pricing.FillToleranceBps
	return p, nil
}
