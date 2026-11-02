package api_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/xJohnNascimento/corridor/internal/store"
)

func TestEveryPaymentKeepsItsOwnTimeline(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	var payments []uuid.UUID
	for _, key := range []string{"k-1", "k-2"} {
		q, err := h.quote("acme", key, "CAD", "USD", "1000000")
		if err != nil {
			t.Fatal(err)
		}
		p, _, err := h.store.Admit(ctx, store.AdmitParams{
			CustomerID:  "acme",
			RequestKey:  key,
			RequestHash: store.Hash(key),
			QuoteID:     uuid.MustParse(q.Id),
			Beneficiary: store.Beneficiary{Name: "Northwind Traders", Account: "000123456789", BankCode: "NWBKUS33", Country: "US"},
			Policy:      map[string]string{},
		})
		if err != nil {
			t.Fatal(err)
		}
		converting := store.Transition{Key: "converting", From: []string{"ADMITTED"}, To: "CONVERTING", Event: "CONVERTING"}
		if err := h.store.TransitionPayment(ctx, p.ID, converting); err != nil {
			t.Fatal(err)
		}
		payments = append(payments, p.ID)
	}
	for _, id := range payments {
		events, err := h.store.Timeline(ctx, id, false)
		if err != nil {
			t.Fatal(err)
		}
		if len(events) != 2 || events[0].Kind != "ADMITTED" || events[1].Kind != "CONVERTING" {
			t.Errorf("payment %s has timeline %+v, want ADMITTED then CONVERTING", id, events)
		}
	}
}
