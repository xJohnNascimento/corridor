package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"go.yaml.in/yaml/v3"

	ledgerv1 "github.com/xJohnNascimento/ledger/api/ledger/v1"
)

type manifest struct {
	ProviderCash map[string]int64 `yaml:"provider_cash"`
	Customers    []struct {
		ID     string           `yaml:"id"`
		Name   string           `yaml:"name"`
		KeyEnv string           `yaml:"key_env"`
		Funds  map[string]int64 `yaml:"funds"`
	} `yaml:"customers"`
}

// seed loads the manifest with explicit funding events, never opening balances: each one is a
// Ditto funding movement and a ledger journal under the same reference, so replaying it changes
// nothing. It ends by proving that provider cash in the ledger equals Ditto's balance.
func seed(ctx context.Context, s *stack, path string) error {
	data, err := os.ReadFile(path) //nolint:gosec // the manifest is the operator's own --manifest argument
	if err != nil {
		return err
	}
	var m manifest
	if err := yaml.Unmarshal(data, &m); err != nil {
		return err
	}
	for _, currency := range currencies {
		equity, err := s.ensure(ctx, ledgerv1.OwnerKind_OWNER_KIND_HOUSE, houseOwner, "funding_equity", currency)
		if err != nil {
			return err
		}
		if err := s.fund(ctx, "seed-liquidity-"+currency, currency, m.ProviderCash[currency], equity); err != nil {
			return err
		}
	}
	for _, c := range m.Customers {
		key := os.Getenv(c.KeyEnv)
		if key == "" {
			return fmt.Errorf("%s is not set; run through make, which loads .env", c.KeyEnv)
		}
		if _, err := s.fxWithEnv(ctx, []string{"FX_CUSTOMER_KEY=" + key}, "customers", "upsert", "--id", c.ID, "--name", c.Name); err != nil {
			return err
		}
		for _, currency := range currencies {
			amount := c.Funds[currency]
			if amount == 0 {
				continue
			}
			funds, err := s.ensure(ctx, ledgerv1.OwnerKind_OWNER_KIND_CUSTOMER, c.ID, "funds", currency)
			if err != nil {
				return err
			}
			if err := s.fund(ctx, fmt.Sprintf("seed-customer-%s-%s", c.ID, currency), currency, amount, funds); err != nil {
				return err
			}
		}
	}
	return checkProviderCash(ctx, s)
}

func checkProviderCash(ctx context.Context, s *stack) error {
	ditto, err := s.providerBalances(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, currency := range currencies {
		cash, err := s.ensure(ctx, ledgerv1.OwnerKind_OWNER_KIND_PROVIDER, providerOwner, "cash", currency)
		if err != nil {
			return err
		}
		ledger, err := s.balance(ctx, cash)
		if err != nil {
			return err
		}
		fmt.Printf("provider cash %s: ledger %d, ditto %d\n", currency, ledger, ditto[currency])
		if ledger != ditto[currency] {
			errs = append(errs, fmt.Errorf("provider cash %s: ledger %d, ditto %d", currency, ledger, ditto[currency]))
		}
	}
	return errors.Join(errs...)
}
