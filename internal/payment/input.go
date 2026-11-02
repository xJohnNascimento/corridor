package payment

import (
	"encoding/json"

	"github.com/xJohnNascimento/corridor/internal/config"
	"github.com/xJohnNascimento/corridor/internal/store"
)

// InputFrom builds a workflow input from the stored payment, so the API's fast path and the
// dispatcher start the same workflow with the same arguments.
func InputFrom(p store.Payment) (Input, error) {
	var policy config.Policy
	if err := json.Unmarshal(p.Policy, &policy); err != nil {
		return Input{}, err
	}
	q := p.Quote
	return Input{
		PaymentID:  p.ID.String(),
		CustomerID: p.CustomerID,
		PayoutID:   p.PayoutID.String(),
		Policy:     policy,
		Quote: QuoteSnapshot{
			SellCurrency: q.SellCurrency, BuyCurrency: q.BuyCurrency, Sell: q.SellAmount, Fee: q.Fee, Net: q.NetSell,
			ProviderBuy: q.ProviderBuy, Buy: q.Buy, Spread: q.Spread, ProviderQuoteID: q.ProviderQuoteID, ProviderExpiresAt: q.ProviderExpiresAt,
		},
	}, nil
}
