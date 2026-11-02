// Package verification checks V1 to V11 across the ledger, Corridor and the provider. It reads
// only their exports and shares wire definitions with them, never posting or pricing code; its
// expectations come from its own integer folds.
package verification

import (
	ledgerv1 "github.com/xJohnNascimento/ledger/api/ledger/v1"

	corridorv1 "github.com/xJohnNascimento/corridor/api/corridor/v1"
)

type Dataset struct {
	Ledger   Ledger
	Corridor Corridor
	Provider Provider
	// Running holds, for each payment that is not terminal, whether Temporal shows its workflow
	// running. Temporal visibility alone never proves a payment exists.
	Running    map[string]bool
	Incomplete []string
	Manifests  []Manifest
}

type Manifest struct {
	Source      string
	CapturedAt  string
	ContentHash string
	Counts      map[string]int64
}

type Ledger struct {
	Accounts     map[string]*ledgerv1.Account
	Balances     map[string]*ledgerv1.Balance
	Entries      []*ledgerv1.JournalEntry
	Reservations []*ledgerv1.Reservation
}

type Corridor struct {
	Payments  []*corridorv1.Payment
	Trades    []*corridorv1.TradeRecord
	Evidence  []*corridorv1.EvidenceRecord
	Intents   []*corridorv1.LedgerIntentRecord
	Commands  []*corridorv1.Command
	Proposals []*corridorv1.ProposalRecord
}

type Provider struct {
	Currencies []ProviderCurrency
	Movements  []Movement
	Outcomes   []Outcome
}

type ProviderCurrency struct {
	Currency     string
	Opening      int64
	Closing      int64
	FromSequence int64
	ToSequence   int64
}

type Movement struct {
	Currency     string
	Sequence     int64
	Amount       int64
	BalanceAfter int64
	Kind         string
	Reference    string
}

type Outcome struct {
	Operation string
	Reference string
	Status    string
	Currency  string
	Amount    int64
}

type Finding struct {
	Invariant string
	Subject   string
	Detail    string
}
