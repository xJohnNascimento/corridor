package payment

import (
	"time"

	"github.com/xJohnNascimento/corridor/internal/config"
	"github.com/xJohnNascimento/corridor/internal/store"
)

const (
	TaskQueue         = "fx"
	ProviderTaskQueue = "fx-provider"

	UpdateAwaitConversion = "awaitConversion"
	UpdateExecuteCommand  = "executeCommand"
	SignalProviderHint    = "providerHint"

	ErrorLedgerUnexpected   = string(ReviewLedgerUnexpected)
	ErrorContractBreach     = string(ReviewProviderContractBreach)
	ErrorPayoutProcessing   = "PAYOUT_PROCESSING"
	ErrorTransitionConflict = "TRANSITION_CONFLICT"
	ErrorInvalidArgument    = "INVALID_ARGUMENT"
)

// ReviewReason is why a payment is in review. Each reason is also an error type, so a failed
// activity that opens a review carries its own reason.
type ReviewReason = store.ReviewReason

const (
	ReviewFillOutOfTolerance          = store.ReviewFillOutOfTolerance
	ReviewPayoutUnconfirmed           = store.ReviewPayoutUnconfirmed
	ReviewConflictingProviderEvidence = store.ReviewConflictingProviderEvidence
	ReviewProviderContractBreach      = store.ReviewProviderContractBreach
	ReviewLedgerUnexpected            = store.ReviewLedgerUnexpected
	ReviewRecoveryRequired            = store.ReviewRecoveryRequired
)

func WorkflowID(paymentID string) string {
	return "payment/" + paymentID
}

// Input holds ids and priced numbers only: credentials and beneficiary details never enter
// workflow history.
type Input struct {
	PaymentID  string
	CustomerID string
	PayoutID   string
	Quote      QuoteSnapshot
	Policy     config.Policy
}

type QuoteSnapshot struct {
	SellCurrency      string
	BuyCurrency       string
	Sell              int64
	Fee               int64
	Net               int64
	ProviderBuy       int64
	Buy               int64
	Spread            int64
	ProviderQuoteID   string
	ProviderExpiresAt time.Time
}

// Snapshot is the durable state a run resumes from, so a new run never writes below it.
type Snapshot struct {
	PaymentStatus     PaymentStatus
	PayoutStatus      PayoutStatus
	Attempt           int
	TradeStatus       string
	CancelCommandID   string
	ReviewReason      ReviewReason
	ReleaseAt         *time.Time
	DeliveredAmount   int64
	SubmissionStarted bool
}

type Reserved struct {
	ReservationID string
	Refused       bool
	OnProvider    bool
}

type TradeOutcome struct {
	Status    string
	Delivered int64
	Matches   bool
}

type PayoutOutcome struct {
	Status      string
	FailureCode string
	EvidenceID  string
	Matches     bool
}

type SubmitResult string

const (
	SubmitMoved     SubmitResult = "MOVED"
	SubmitCancelled SubmitResult = "CANCELLED"
	SubmitEarly     SubmitResult = "EARLY"
)

type Hint struct {
	EventID   string
	Reference string
	Status    string
	Sequence  int
}

type Decision struct {
	CommandID string
	State     string
	Reason    string
	Cancel    bool
	Approval  *Approval
}

type Approval struct {
	ProposalID string
	CommandID  string
	Decision   string
	Delivered  int64
	EvidenceID string
}

type Applied struct {
	Done          bool
	PaymentStatus PaymentStatus
	PayoutStatus  PayoutStatus
}

type PrepareResult struct {
	Result          SubmitResult
	CancelCommandID string
}
