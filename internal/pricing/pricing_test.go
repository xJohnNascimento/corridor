package pricing_test

import (
	"math"
	"math/big"
	"testing"

	"pgregory.net/rapid"

	"github.com/xJohnNascimento/corridor/internal/pricing"
)

func TestTheWorkedExamplePricesExactly(t *testing.T) {
	schedule := pricing.Schedule{FeeBps: 50, SpreadBps: 5}
	fee, net, err := schedule.Net(1000000)
	if err != nil || fee != 5000 || net != 995000 {
		t.Fatalf("fee %d net %d (%v), want 5000 and 995000", fee, net, err)
	}
	q, err := schedule.Price(1000000, 73500000, 731325)
	if err != nil {
		t.Fatal(err)
	}
	want := pricing.Quote{Sell: 1000000, Fee: 5000, NetSell: 995000, ProviderRateE8: 73500000, CustomerRateE8: 73463250, ProviderBuy: 731325, Buy: 730959, Spread: 366}
	if q != want {
		t.Fatalf("got %+v\nwant %+v", q, want)
	}
}

func TestPriceRefusesAProviderAmountThatDoesNotMatchItsRate(t *testing.T) {
	if _, err := (pricing.Schedule{FeeBps: 50, SpreadBps: 5}).Price(1000000, 73500000, 731326); err == nil {
		t.Fatal("a provider amount off its own rate was accepted")
	}
}

func TestPricingIdentitiesHoldForEveryInput(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		schedule := pricing.Schedule{FeeBps: rapid.Int64Range(0, 500).Draw(t, "fee"), SpreadBps: rapid.Int64Range(0, 100).Draw(t, "spread")}
		sell := rapid.Int64Range(10000, 25_000_000).Draw(t, "sell")
		rate := rapid.Int64Range(1_000_000, 1_000_000_000).Draw(t, "rate")
		_, net, err := schedule.Net(sell)
		if err != nil {
			t.Fatal(err)
		}
		q, err := schedule.Price(sell, rate, net*rate/100_000_000)
		if err != nil {
			t.Skip("converts to nothing")
		}
		if q.NetSell != q.Sell-q.Fee || q.Spread != q.ProviderBuy-q.Buy || q.Spread < 0 || q.Fee*10000 < sell*schedule.FeeBps {
			t.Fatalf("identities broken: %+v", q)
		}
		if q.Fee*10000-sell*schedule.FeeBps >= 10000 {
			t.Fatalf("the fee rounds up by a whole unit or more: %+v", q)
		}
	})
}

func TestToleranceIsTenBasisPointsOfTheQuotedAmount(t *testing.T) {
	tolerance := pricing.Tolerance{Bps: 10}
	for delivered, within := range map[int64]bool{
		731325: true, 730594: true, 730593: false, 732056: true, 732057: false,
		1_000_000_000_000_000: false, math.MaxInt64: false,
	} {
		if got := tolerance.Within(731325, delivered); got != within {
			t.Errorf("delivered %d against 731325: within %v, want %v", delivered, got, within)
		}
	}
}

func TestToleranceAgreesWithExactArithmetic(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		tolerance := pricing.Tolerance{Bps: rapid.Int64Range(0, 1_000).Draw(t, "bps")}
		quoted := rapid.Int64Range(1, 1<<53).Draw(t, "quoted")
		delivered := rapid.Int64Range(1, math.MaxInt64).Draw(t, "delivered")
		diff := new(big.Int).Abs(new(big.Int).Sub(big.NewInt(delivered), big.NewInt(quoted)))
		limit := new(big.Int).Mul(big.NewInt(quoted), big.NewInt(tolerance.Bps))
		want := new(big.Int).Mul(diff, big.NewInt(10_000)).Cmp(limit) <= 0
		if got := tolerance.Within(quoted, delivered); got != want {
			t.Fatalf("delivered %d against %d at %d bps: within %v, want %v", delivered, quoted, tolerance.Bps, got, want)
		}
	})
}
