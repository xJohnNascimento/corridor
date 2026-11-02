package main

// The worked example in minor units. CAD 10,000.00 is sold at a 0.50% fee; the provider quotes
// the net CAD 9,950.00 at USD 7,313.25, and the customer receives that less a 0.05% spread.
const (
	quoteSell   = 1000000
	quoteFee    = 5000
	quoteNet    = quoteSell - quoteFee
	providerBuy = 731325
	customerBuy = 730959
	quoteSpread = providerBuy - customerBuy
)

// A fill short by more than the 10 bps tolerance on this quote (USD 7.31) needs review; a smaller
// difference, in either direction, is booked to the difference account.
const (
	beyondTolerance = 1000
	withinTolerance = 500
)
