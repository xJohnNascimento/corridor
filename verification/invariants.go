package verification

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	ledgerv1 "github.com/xJohnNascimento/ledger/api/ledger/v1"

	corridorv1 "github.com/xJohnNascimento/corridor/api/corridor/v1"
)

var invariants = []struct {
	ID    string
	Title string
	Check func(*index) []Finding
}{
	{"V1", "every journal entry has 2 to 50 postings and balances per asset; every posting's account exists, belongs to the entry's ledger and has the posting's asset", v1},
	{"V2", "every posted balance equals the fold of its postings; each account's posting sequences run from 1 with no gap or duplicate", v2},
	{"V3", "every reserved balance equals the sum of its active reservation legs; a reservation is final exactly when it has a final command", v3},
	{"V4", "no enforced account has negative availability: posted minus reserved plus overdraft limit", v4},
	{"V5", "every committed conversion has one matching provider fill and one settlement entry whose net provider-cash leg equals the delivered amount", v5},
	{"V6", "every completed payout has one matching COMPLETED provider outcome, one provider payment, one settlement entry and no return", v6},
	{"V7", "every failed payout, and every cancelled payout that was submitted, has a no-payment provider outcome for its reference and terms; a cancelled payout never submitted has no provider operation; each has no provider debit, no unreleased reservation and one return crediting the paying customer's funds", v7},
	{"V8", "every payment's clearing balance equals its bought amount while it is converted or paying out, and zero otherwise", v8},
	{"V9", "provider balances roll forward per currency, and each reference's provider net equals the ledger's provider-cash net unless its payment is still in flight", v9},
	{"V10", "every unfinished payment has a running workflow or a recovery review; a finished payment has no RECEIVED or ACCEPTED command and no unobserved ledger intent", v10},
	{"V11", "every approved proposal has distinct maker and approver, a matching digest, an approval command for its payment and unchanged cited evidence", v11},
}

// InvariantIDs lists the invariants Verify runs, in order.
func InvariantIDs() []string {
	ids := make([]string, len(invariants))
	for i, inv := range invariants {
		ids[i] = inv.ID
	}
	return ids
}

type (
	productKey struct{ product, kind string }
	outcomeKey struct{ operation, reference string }
	refKey     struct{ namespace, reference string }
)

// index holds the lookups every check shares, built once so that no check scans the dataset
// inside a per-payment loop.
type index struct {
	d            *Dataset
	entries      map[string]*ledgerv1.JournalEntry
	byProduct    map[productKey][]*ledgerv1.JournalEntry
	outcomes     map[outcomeKey][]Outcome
	debits       map[string]int
	reservations map[refKey][]*ledgerv1.Reservation
	// operations counts a reference's provider outcomes and movements, whatever their kind.
	operations map[string]int
	// customerFunds maps a customer and an asset to the customer's funds account.
	customerFunds map[ownerAsset]string
	malformed     map[string]bool
}

type ownerAsset struct{ owner, asset string }

func newIndex(d *Dataset) *index {
	x := &index{
		d:             d,
		entries:       make(map[string]*ledgerv1.JournalEntry, len(d.Ledger.Entries)),
		byProduct:     map[productKey][]*ledgerv1.JournalEntry{},
		outcomes:      map[outcomeKey][]Outcome{},
		debits:        map[string]int{},
		reservations:  map[refKey][]*ledgerv1.Reservation{},
		operations:    map[string]int{},
		customerFunds: map[ownerAsset]string{},
		malformed:     map[string]bool{},
	}
	for id, a := range d.Ledger.Accounts {
		if a.OwnerKind == ledgerv1.OwnerKind_OWNER_KIND_CUSTOMER && a.Purpose == "funds" {
			x.customerFunds[ownerAsset{a.OwnerId, a.AssetCode}] = id
		}
	}
	for _, e := range d.Ledger.Entries {
		if _, seen := x.entries[e.Id]; !seen {
			x.entries[e.Id] = e
		}
		key := productKey{e.ProductReference, e.Kind}
		x.byProduct[key] = append(x.byProduct[key], e)
	}
	for _, o := range d.Provider.Outcomes {
		key := outcomeKey{o.Operation, o.Reference}
		x.outcomes[key] = append(x.outcomes[key], o)
		x.operations[o.Reference]++
	}
	for _, m := range d.Provider.Movements {
		x.operations[m.Reference]++
		switch m.Kind {
		case "PAYOUT_DEBIT":
			x.debits[m.Reference]++
		case "PAYOUT_REFUND":
			x.debits[m.Reference]--
		}
	}
	for _, r := range d.Ledger.Reservations {
		key := refKey{r.ReservationRef.GetNamespace(), r.ReservationRef.GetReference()}
		x.reservations[key] = append(x.reservations[key], r)
	}
	return x
}

// amount parses a decimal amount. A malformed one is remembered, so that the report is
// INCOMPLETE instead of reading it as zero and letting an invariant pass.
func (x *index) amount(s string) int64 {
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		x.malformed[strconv.Quote(s)] = true
	}
	return v
}

func (x *index) malformedReasons() []string {
	var reasons []string
	for _, value := range sortedKeys(x.malformed) {
		reasons = append(reasons, "malformed amount "+value)
	}
	return reasons
}

func sign(side ledgerv1.Side) int64 {
	if side == ledgerv1.Side_SIDE_DEBIT {
		return 1
	}
	return -1
}

func v1(x *index) []Finding {
	var out []Finding
	for _, e := range x.d.Ledger.Entries {
		perAsset := map[string]int64{}
		if n := len(e.Postings); n < 2 || n > 50 {
			out = append(out, Finding{"V1", "entry " + e.Id, fmt.Sprintf("%d postings", n)})
		}
		for _, p := range e.Postings {
			account, ok := x.d.Ledger.Accounts[p.AccountId]
			if !ok || account.LedgerId != e.LedgerId || account.AssetCode != p.AssetCode {
				out = append(out, Finding{"V1", "entry " + e.Id, "posting on account " + p.AccountId + " breaks membership"})
			}
			perAsset[p.AssetCode] += sign(p.Side) * x.amount(p.Amount)
		}
		for _, asset := range sortedKeys(perAsset) {
			if perAsset[asset] != 0 {
				out = append(out, Finding{"V1", "entry " + e.Id, fmt.Sprintf("off by %d in %s", perAsset[asset], asset)})
			}
		}
	}
	return out
}

func v2(x *index) []Finding {
	fold, sequences := map[string]int64{}, map[string][]int64{}
	for _, e := range x.d.Ledger.Entries {
		for _, p := range e.Postings {
			account := x.d.Ledger.Accounts[p.AccountId]
			if account == nil {
				continue
			}
			v := x.amount(p.Amount)
			if p.Side != account.NormalSide {
				v = -v
			}
			fold[p.AccountId] += v
			sequences[p.AccountId] = append(sequences[p.AccountId], p.AccountSequence)
		}
	}
	var out []Finding
	for _, id := range sortedKeys(x.d.Ledger.Balances) {
		b := x.d.Ledger.Balances[id]
		if x.amount(b.Posted) != fold[id] {
			out = append(out, Finding{"V2", "account " + id, fmt.Sprintf("postings fold to %d, balance says %s", fold[id], b.Posted)})
		}
		seq := sequences[id]
		slices.Sort(seq)
		for i, s := range seq {
			if s != int64(i+1) {
				out = append(out, Finding{"V2", "account " + id, fmt.Sprintf("sequences are not 1 to %d", len(seq))})
				break
			}
		}
	}
	return out
}

func v3(x *index) []Finding {
	reserved := map[string]int64{}
	var out []Finding
	for _, r := range x.d.Ledger.Reservations {
		final := r.State != ledgerv1.ReservationState_RESERVATION_STATE_ACTIVE
		if final != (r.FinalCommandId != "") {
			out = append(out, Finding{"V3", "reservation " + r.Id, "final state and final command disagree"})
		}
		if final {
			continue
		}
		for _, leg := range r.Legs {
			reserved[leg.AccountId] += x.amount(leg.MaxReduction)
		}
	}
	for _, id := range sortedKeys(x.d.Ledger.Balances) {
		if got := x.amount(x.d.Ledger.Balances[id].Reserved); got != reserved[id] {
			out = append(out, Finding{"V3", "account " + id, fmt.Sprintf("active legs sum to %d, balance says %d", reserved[id], got)})
		}
	}
	return out
}

func v4(x *index) []Finding {
	var out []Finding
	for _, id := range sortedKeys(x.d.Ledger.Balances) {
		b, account := x.d.Ledger.Balances[id], x.d.Ledger.Accounts[id]
		if account == nil || !account.EnforceAvailable {
			continue
		}
		if available := x.amount(b.Posted) - x.amount(b.Reserved) + x.amount(b.OverdraftLimit); available < 0 {
			out = append(out, Finding{"V4", "account " + id, fmt.Sprintf("available %d", available)})
		}
	}
	return out
}

// providerCashNet sums an entry's provider-cash legs per currency, in the account's debit-normal
// sign, which is the provider's own sign for Corridor's balance.
func (x *index) providerCashNet(e *ledgerv1.JournalEntry) map[string]int64 {
	net := map[string]int64{}
	for _, p := range e.Postings {
		account := x.d.Ledger.Accounts[p.AccountId]
		if account != nil && account.Purpose == "cash" {
			net[p.AssetCode] += sign(p.Side) * x.amount(p.Amount)
		}
	}
	return net
}

func (x *index) entriesFor(product, kind string) []*ledgerv1.JournalEntry {
	return x.byProduct[productKey{product, kind}]
}

func (x *index) outcomesFor(operation, reference string) []Outcome {
	return x.outcomes[outcomeKey{operation, reference}]
}

func v5(x *index) []Finding {
	var out []Finding
	for _, t := range x.d.Corridor.Trades {
		if t.EntryId == "" {
			continue
		}
		subject := "trade " + t.ProviderReference
		delivered := x.amount(t.GetDelivered().GetAmount())
		fills := x.outcomesFor("TRADE", t.ProviderReference)
		if len(fills) != 1 || fills[0].Status != "FILLED" || fills[0].Amount != delivered {
			out = append(out, Finding{"V5", subject, fmt.Sprintf("%d provider outcomes for a delivered %s", len(fills), t.GetDelivered().GetAmount())})
		}
		e := x.entries[t.EntryId]
		if e == nil || len(x.entriesFor("payment/"+t.PaymentId, "fx.conversion")) != 1 {
			out = append(out, Finding{"V5", subject, "no single settlement entry"})
			continue
		}
		if net := x.providerCashNet(e)[t.GetDelivered().GetCurrency()]; net != delivered {
			out = append(out, Finding{"V5", subject, fmt.Sprintf("the entry moves %d provider %s, the provider delivered %s", net, t.GetDelivered().GetCurrency(), t.GetDelivered().GetAmount())})
		}
	}
	return out
}

func v6(x *index) []Finding {
	var out []Finding
	for _, p := range x.d.Corridor.Payments {
		if p.GetPayout().GetStatus() != corridorv1.PayoutStatus_PAYOUT_STATUS_COMPLETED {
			continue
		}
		id := p.GetPayout().GetId()
		paid := x.outcomesFor("PAYOUT", id)
		if len(paid) != 1 || paid[0].Status != "COMPLETED" || paid[0].Amount != x.amount(p.GetPayout().GetAmount().GetAmount()) || paid[0].Currency != p.GetPayout().GetAmount().GetCurrency() {
			out = append(out, Finding{"V6", "payout " + id, fmt.Sprintf("%d provider outcomes for a completed payout", len(paid))})
		}
		if n := x.debits[id]; n != 1 {
			out = append(out, Finding{"V6", "payout " + id, fmt.Sprintf("the provider paid %d times", n)})
		}
		if n := len(x.entriesFor("payout/"+id, "fx.payout")); n != 1 {
			out = append(out, Finding{"V6", "payout " + id, fmt.Sprintf("%d settlement entries", n)})
		}
		if n := len(x.entriesFor("payout/"+id, "fx.payout-return")); n != 0 {
			out = append(out, Finding{"V6", "payout " + id, fmt.Sprintf("%d returns next to a successful payout", n)})
		}
	}
	return out
}

func v7(x *index) []Finding {
	var out []Finding
	for _, p := range x.d.Corridor.Payments {
		s := p.GetPayout().GetStatus()
		if s != corridorv1.PayoutStatus_PAYOUT_STATUS_FAILED && s != corridorv1.PayoutStatus_PAYOUT_STATUS_CANCELLED {
			continue
		}
		id := p.GetPayout().GetId()
		unsent := s == corridorv1.PayoutStatus_PAYOUT_STATUS_CANCELLED && p.GetPayout().GetSubmissionStartedAt() == nil
		switch {
		case unsent:
			if n := x.operations[id]; n != 0 {
				out = append(out, Finding{"V7", "payout " + id, fmt.Sprintf("the provider has %d operations for a payout that was never submitted", n)})
			}
		case !x.provenUnpaid(p.GetPayout()):
			out = append(out, Finding{"V7", "payout " + id, "no provider outcome for its terms shows that nothing was paid"})
		}
		for _, o := range x.outcomesFor("PAYOUT", id) {
			if o.Status == "COMPLETED" {
				out = append(out, Finding{"V7", "payout " + id, "the provider shows a completed payment"})
			}
		}
		if n := x.debits[id]; n != 0 {
			out = append(out, Finding{"V7", "payout " + id, fmt.Sprintf("the provider kept %d debits", n)})
		}
		for _, r := range x.reservations[refKey{"corridor.payout", id}] {
			if r.State != ledgerv1.ReservationState_RESERVATION_STATE_RELEASED {
				out = append(out, Finding{"V7", "payout " + id, "its reservation is " + r.State.String()})
			}
		}
		returns := x.entriesFor("payout/"+id, "fx.payout-return")
		if len(returns) != 1 {
			out = append(out, Finding{"V7", "payout " + id, fmt.Sprintf("%d returns", len(returns))})
			continue
		}
		if detail := x.returnCredit(p, returns[0]); detail != "" {
			out = append(out, Finding{"V7", "payout " + id, detail})
		}
	}
	return out
}

// provenUnpaid reports whether the provider shows, for this payout's reference, a final outcome
// that means no payment. An outcome may omit its terms, as a fence on an unknown reference does,
// but a currency or amount it carries must be the payout's.
func (x *index) provenUnpaid(payout *corridorv1.Payout) bool {
	amount := x.amount(payout.GetAmount().GetAmount())
	for _, o := range x.outcomesFor("PAYOUT", payout.GetId()) {
		switch o.Status {
		case "FAILED", "CANCELLED", "REJECTED":
		default:
			continue
		}
		if (o.Currency == "" || o.Currency == payout.GetAmount().GetCurrency()) && (o.Amount == 0 || o.Amount == amount) {
			return true
		}
	}
	return false
}

// returnCredit checks that the return gives the payout's amount back to the funds account of the
// customer whose funds the payment's conversion debited. The export carries no customer on the
// payment, so the conversion entry is what names the customer.
func (x *index) returnCredit(p *corridorv1.Payment, entry *ledgerv1.JournalEntry) string {
	conversions := x.entriesFor("payment/"+p.Id, "fx.conversion")
	if len(conversions) != 1 {
		return "no single conversion entry names the paying customer"
	}
	var owner string
	for _, posting := range conversions[0].Postings {
		account := x.d.Ledger.Accounts[posting.AccountId]
		if posting.Side == ledgerv1.Side_SIDE_DEBIT && account != nil && account.OwnerKind == ledgerv1.OwnerKind_OWNER_KIND_CUSTOMER && account.Purpose == "funds" {
			owner = account.OwnerId
		}
	}
	currency := p.GetPayout().GetAmount().GetCurrency()
	funds, ok := x.customerFunds[ownerAsset{owner, currency}]
	if !ok {
		return "the paying customer has no " + currency + " funds account"
	}
	var credited int64
	for _, posting := range entry.Postings {
		if posting.AccountId == funds && posting.Side == ledgerv1.Side_SIDE_CREDIT {
			credited += x.amount(posting.Amount)
		}
	}
	if want := x.amount(p.GetPayout().GetAmount().GetAmount()); credited != want {
		return fmt.Sprintf("the return credits %d to the paying customer's %s funds, want %d", credited, currency, want)
	}
	return ""
}

func terminal(s corridorv1.PaymentStatus) bool {
	switch s {
	case corridorv1.PaymentStatus_PAYMENT_STATUS_COMPLETED, corridorv1.PaymentStatus_PAYMENT_STATUS_REJECTED, corridorv1.PaymentStatus_PAYMENT_STATUS_EXPIRED,
		corridorv1.PaymentStatus_PAYMENT_STATUS_PAYOUT_FAILED, corridorv1.PaymentStatus_PAYMENT_STATUS_PAYOUT_CANCELLED:
		return true
	default:
		return false
	}
}

func v8(x *index) []Finding {
	clearing := map[string]string{}
	for id, a := range x.d.Ledger.Accounts {
		if a.Purpose == "clearing" {
			clearing[a.OwnerId] = id
		}
	}
	var out []Finding
	for _, p := range x.d.Corridor.Payments {
		var balance int64
		if id, ok := clearing[p.Id]; ok && x.d.Ledger.Balances[id] != nil {
			balance = x.amount(x.d.Ledger.Balances[id].Posted)
		}
		want := int64(0)
		if p.Status == corridorv1.PaymentStatus_PAYMENT_STATUS_CONVERTED || p.Status == corridorv1.PaymentStatus_PAYMENT_STATUS_PAYING_OUT {
			want = x.amount(p.GetBuy().GetAmount())
		}
		if balance != want {
			out = append(out, Finding{"V8", "payment " + p.Id, fmt.Sprintf("clearing holds %d at %s, want %d", balance, p.Status, want)})
		}
	}
	return out
}

// v9 rolls each provider currency forward and matches the provider's net per reference with the
// ledger's provider-cash net per product reference. A gap on an unfinished payment is in flight,
// not a finding.
func v9(x *index) []Finding {
	var out []Finding
	moved := map[string]int64{}
	for _, m := range x.d.Provider.Movements {
		moved[m.Currency] += m.Amount
	}
	for _, c := range x.d.Provider.Currencies {
		if c.Opening+moved[c.Currency] != c.Closing {
			out = append(out, Finding{"V9", "provider " + c.Currency, fmt.Sprintf("%d plus %d movements is not the closing %d", c.Opening, moved[c.Currency], c.Closing)})
		}
	}
	type key struct{ reference, currency string }
	provider, ledger := map[key]int64{}, map[key]int64{}
	for _, m := range x.d.Provider.Movements {
		provider[key{m.Reference, m.Currency}] += m.Amount
	}
	for _, e := range x.d.Ledger.Entries {
		reference := strings.TrimPrefix(strings.TrimPrefix(e.ProductReference, "payment/"), "payout/")
		for currency, net := range x.providerCashNet(e) {
			if net != 0 {
				ledger[key{reference, currency}] += net
			}
		}
	}
	inFlight := map[string]bool{}
	for _, p := range x.d.Corridor.Payments {
		if !terminal(p.Status) {
			inFlight[p.Id], inFlight[p.GetPayout().GetId()] = true, true
		}
	}
	keys := map[key]bool{}
	for k := range provider {
		keys[k] = true
	}
	for k := range ledger {
		keys[k] = true
	}
	sorted := make([]key, 0, len(keys))
	for k := range keys {
		sorted = append(sorted, k)
	}
	slices.SortFunc(sorted, func(a, b key) int {
		return cmp.Or(cmp.Compare(a.reference, b.reference), cmp.Compare(a.currency, b.currency))
	})
	for _, k := range sorted {
		if provider[k] != ledger[k] && !inFlight[k.reference] {
			out = append(out, Finding{"V9", "reference " + k.reference, fmt.Sprintf("the provider moved %d %s, the ledger %d", provider[k], k.currency, ledger[k])})
		}
	}
	return out
}

func v10(x *index) []Finding {
	var out []Finding
	finished := map[string]bool{}
	for _, p := range x.d.Corridor.Payments {
		if terminal(p.Status) {
			finished[p.Id] = true
			continue
		}
		if !x.d.Running[p.Id] && p.ReviewReason != corridorv1.ReviewReason_REVIEW_REASON_RECOVERY_REQUIRED {
			out = append(out, Finding{"V10", "payment " + p.Id, "no running workflow and no recovery review"})
		}
	}
	for _, c := range x.d.Corridor.Commands {
		pending := c.State == corridorv1.CommandState_COMMAND_STATE_RECEIVED || c.State == corridorv1.CommandState_COMMAND_STATE_ACCEPTED
		if pending && finished[c.SubjectId] {
			out = append(out, Finding{"V10", "command " + c.Id, "still " + c.State.String() + " for a finished payment"})
		}
	}
	for _, i := range x.d.Corridor.Intents {
		if i.ObservedResult == "" && finished[i.PaymentId] {
			out = append(out, Finding{"V10", "intent " + i.Namespace + "/" + i.Reference, "never observed for a finished payment"})
		}
	}
	return out
}

// proposalDigest is the verifier's own canonical form of a proposal, field for field.
func proposalDigest(r *corridorv1.ProposalRecord) (string, error) {
	encoded, err := json.Marshal(struct {
		PaymentID         string `json:"payment_id"`
		SubjectKind       string `json:"subject_kind"`
		SubjectID         string `json:"subject_id"`
		ReviewReason      string `json:"review_reason"`
		ReviewRevision    int32  `json:"review_revision"`
		Decision          string `json:"decision"`
		ProviderReference string `json:"provider_reference"`
		Currency          string `json:"currency"`
		Amount            int64  `json:"amount"`
		EvidenceID        string `json:"evidence_id"`
		EvidenceDigest    string `json:"evidence_digest"`
		Maker             string `json:"maker"`
		ProposalRevision  int32  `json:"proposal_revision"`
	}{r.PaymentId, r.SubjectKind, r.SubjectId, r.ReviewReason, r.ReviewRevision, r.Decision, r.ProviderReference,
		r.Currency, r.Amount, r.EvidenceId, r.EvidenceDigest, r.Maker, r.ProposalRevision})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

func v11(x *index) []Finding {
	evidence := map[string]*corridorv1.EvidenceRecord{}
	for _, e := range x.d.Corridor.Evidence {
		evidence[e.Id] = e
	}
	commands := map[string]*corridorv1.Command{}
	for _, c := range x.d.Corridor.Commands {
		commands[c.Id] = c
	}
	var out []Finding
	for _, r := range x.d.Corridor.Proposals {
		if r.Approver == "" {
			continue
		}
		subject := "proposal " + r.Id
		if r.Maker == "" || r.Maker == r.Approver {
			out = append(out, Finding{"V11", subject, "maker and checker are not two operators"})
		}
		if digest, err := proposalDigest(r); err != nil || digest != r.Digest {
			out = append(out, Finding{"V11", subject, "the digest does not match the proposal"})
		}
		if c := commands[r.ApprovalCommandId]; c == nil || c.SubjectId != r.PaymentId {
			out = append(out, Finding{"V11", subject, "the approval command is missing or for another payment"})
		}
		if r.EvidenceId != "" && (evidence[r.EvidenceId] == nil || evidence[r.EvidenceId].Digest != r.EvidenceDigest) {
			out = append(out, Finding{"V11", subject, "the cited evidence is missing or changed"})
		}
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
