package pricing

import (
	"errors"
	"fmt"

	"github.com/xJohnNascimento/corridor/internal/money"
)

const bps = 10_000

type Schedule struct {
	FeeBps    int64
	SpreadBps int64
}

type Quote struct {
	Sell           int64
	Fee            int64
	NetSell        int64
	ProviderRateE8 int64
	CustomerRateE8 int64
	ProviderBuy    int64
	Buy            int64
	Spread         int64
}

// Net returns the fee, rounded up, and the amount left to convert. Every rounding favours Corridor
// by less than one minor unit.
func (s Schedule) Net(sell int64) (fee, net int64, err error) {
	fee, err = money.MulDivCeil(sell, s.FeeBps, bps)
	if err != nil {
		return 0, 0, err
	}
	if fee >= sell {
		return 0, 0, errors.New("the fee consumes the whole amount")
	}
	return fee, sell - fee, nil
}

// Price prices a quote from the provider's rate and the amount the provider will deliver for the
// net sold amount, which must be the provider's own floor at that rate.
func (s Schedule) Price(sell, providerRateE8, providerBuy int64) (Quote, error) {
	fee, net, err := s.Net(sell)
	if err != nil {
		return Quote{}, err
	}
	expected, err := money.MulDivFloor(net, providerRateE8, money.RateScale)
	if err != nil {
		return Quote{}, err
	}
	if expected != providerBuy {
		return Quote{}, fmt.Errorf("the provider delivers %d for %d at %d, expected %d", providerBuy, net, providerRateE8, expected)
	}
	customerRate, err := money.MulDivFloor(providerRateE8, bps-s.SpreadBps, bps)
	if err != nil {
		return Quote{}, err
	}
	buy, err := money.MulDivFloor(net, customerRate, money.RateScale)
	if err != nil {
		return Quote{}, err
	}
	if buy <= 0 {
		return Quote{}, errors.New("the amount converts to nothing")
	}
	return Quote{
		Sell: sell, Fee: fee, NetSell: net, ProviderRateE8: providerRateE8, CustomerRateE8: customerRate,
		ProviderBuy: providerBuy, Buy: buy, Spread: providerBuy - buy,
	}, nil
}

type Tolerance struct {
	Bps int64
}

func (t Tolerance) Within(quoted, delivered int64) bool {
	diff := delivered - quoted
	if diff < 0 {
		diff = -diff
	}
	return diff*bps <= quoted*t.Bps
}
