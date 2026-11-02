package api

import (
	"context"
	"errors"
	"log/slog"
	"regexp"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/client"
	"google.golang.org/protobuf/types/known/timestamppb"

	corridorv1 "github.com/xJohnNascimento/corridor/api/corridor/v1"
	"github.com/xJohnNascimento/corridor/internal/dispatch"
	"github.com/xJohnNascimento/corridor/internal/failpoint"
	"github.com/xJohnNascimento/corridor/internal/money"
	"github.com/xJohnNascimento/corridor/internal/payment"
	"github.com/xJohnNascimento/corridor/internal/store"
)

var countryPattern = regexp.MustCompile(`^[A-Z]{2}$`)

type paymentService struct {
	*Server
}

type paymentRequest struct {
	QuoteID        string            `json:"quote_id"`
	Beneficiary    store.Beneficiary `json:"beneficiary"`
	RemittanceInfo string            `json:"remittance_info"`
}

// CreatePayment admits the payment in one transaction, then asks the workflow for the conversion
// decision within FastPathWait. The answer is the durable receipt with the current snapshot;
// Temporal being down never fails it, because the dispatcher guarantees the start.
func (s paymentService) CreatePayment(ctx context.Context, req *connect.Request[corridorv1.CreatePaymentRequest]) (*connect.Response[corridorv1.CreatePaymentResponse], error) {
	actor := actorFrom(ctx)
	key, err := idempotencyKey(req.Header())
	if err != nil {
		return nil, err
	}
	m := req.Msg
	quoteID, err := uuid.Parse(m.QuoteId)
	if err != nil {
		return nil, failure(corridorv1.ErrorReason_ERROR_REASON_VALIDATION_FAILED, "quote_id is not a quote id")
	}
	b := m.Beneficiary
	if b == nil || !between(b.Name, 140) || !between(b.Account, 34) || !between(b.BankCode, 11) || !countryPattern.MatchString(b.Country) || len(m.RemittanceInfo) > 140 {
		return nil, failure(corridorv1.ErrorReason_ERROR_REASON_VALIDATION_FAILED,
			"beneficiary needs a name (140), account (34), bank code (11) and a two-letter country; remittance info holds 140")
	}
	request := paymentRequest{QuoteID: m.QuoteId, Beneficiary: store.Beneficiary{Name: b.Name, Account: b.Account, BankCode: b.BankCode, Country: b.Country}, RemittanceInfo: m.RemittanceInfo}

	p, replay, err := s.store.Admit(ctx, store.AdmitParams{
		CustomerID: actor.ID, RequestKey: key, RequestHash: store.Hash(request), QuoteID: quoteID,
		Beneficiary: request.Beneficiary, RemittanceInfo: m.RemittanceInfo, Policy: s.policy,
	})
	var used *store.QuoteUsedError
	switch {
	case errors.Is(err, store.ErrNotFound):
		return nil, failure(corridorv1.ErrorReason_ERROR_REASON_NOT_FOUND, "quote not found")
	case errors.Is(err, store.ErrQuoteExpired):
		return nil, failure(corridorv1.ErrorReason_ERROR_REASON_QUOTE_EXPIRED, err.Error())
	case errors.As(err, &used):
		return nil, failureWith(corridorv1.ErrorReason_ERROR_REASON_QUOTE_USED, err.Error(), &corridorv1.ErrorDetail{PaymentId: used.PaymentID.String()})
	case errors.Is(err, store.ErrIdempotencyMismatch):
		return nil, failure(corridorv1.ErrorReason_ERROR_REASON_IDEMPOTENCY_MISMATCH, err.Error())
	case err != nil:
		return nil, unavailable(ctx, err)
	}
	if !replay && s.points.Hit(ctx, failpoint.AdmissionAfterCommit, actor.ID) {
		return nil, unavailable(ctx, failpoint.ErrInjected)
	}
	if p.WorkflowStartPending {
		s.fastPath(ctx, p)
		if p, err = s.store.Payment(ctx, p.ID); err != nil {
			return nil, unavailable(ctx, err)
		}
	}
	return connect.NewResponse(&corridorv1.CreatePaymentResponse{
		Receipt: &corridorv1.Receipt{
			PaymentId: p.ID.String(), PayoutId: p.PayoutID.String(), QuoteId: p.QuoteID.String(),
			AdmissionStatus: corridorv1.AdmissionStatus_ADMISSION_STATUS_ADMITTED, AcceptedAt: timestamppb.New(p.AcceptedAt),
		},
		Payment: paymentMessage(p),
	}), nil
}

// fastPath starts the workflow and waits for awaitConversion in one call. Any failure is left to
// the dispatcher, which retries the start until Temporal accepts it.
func (s paymentService) fastPath(ctx context.Context, p store.Payment) {
	if s.temporal == nil {
		return
	}
	in, err := payment.InputFrom(p)
	if err != nil {
		slog.ErrorContext(ctx, "fast path input", "payment", p.ID, "error", err)
		return
	}
	ctx, cancel := context.WithTimeout(ctx, s.policy.FastPathWait+time.Second)
	defer cancel()
	start := s.temporal.NewWithStartWorkflowOperation(client.StartWorkflowOptions{
		ID:                       payment.WorkflowID(p.ID.String()),
		TaskQueue:                payment.TaskQueue,
		WorkflowIDReusePolicy:    enumspb.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE,
		WorkflowIDConflictPolicy: enumspb.WORKFLOW_ID_CONFLICT_POLICY_USE_EXISTING,
	}, payment.PaymentWorkflow, in)
	handle, err := s.temporal.UpdateWithStartWorkflow(ctx, client.UpdateWithStartWorkflowOptions{
		StartWorkflowOperation: start,
		UpdateOptions: client.UpdateWorkflowOptions{
			UpdateID: "admission/" + p.ID.String(), UpdateName: payment.UpdateAwaitConversion, WaitForStage: client.WorkflowUpdateStageCompleted,
		},
	})
	if err != nil {
		slog.WarnContext(ctx, "fast path start", "payment", p.ID, "error", err)
		return
	}
	if _, err := start.Get(ctx); err != nil {
		slog.WarnContext(ctx, "fast path start result", "payment", p.ID, "error", err)
	} else if err := s.store.ClearStartPending(ctx, p.ID); err != nil {
		slog.WarnContext(ctx, "clear start pending", "payment", p.ID, "error", err)
	}
	var status string
	if err := handle.Get(ctx, &status); err != nil {
		slog.WarnContext(ctx, "fast path conversion decision", "payment", p.ID, "error", err)
	}
}

func (s paymentService) GetPayment(ctx context.Context, req *connect.Request[corridorv1.GetPaymentRequest]) (*connect.Response[corridorv1.GetPaymentResponse], error) {
	p, err := s.customerPayment(ctx, req.Msg.PaymentId)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&corridorv1.GetPaymentResponse{Payment: paymentMessage(p)}), nil
}

func (s paymentService) GetTimeline(ctx context.Context, req *connect.Request[corridorv1.GetTimelineRequest]) (*connect.Response[corridorv1.GetTimelineResponse], error) {
	p, err := s.customerPayment(ctx, req.Msg.PaymentId)
	if err != nil {
		return nil, err
	}
	events, err := s.store.Timeline(ctx, p.ID, false)
	if err != nil {
		return nil, unavailable(ctx, err)
	}
	resp := &corridorv1.GetTimelineResponse{}
	for _, e := range events {
		resp.Events = append(resp.Events, &corridorv1.TimelineEvent{Kind: e.Kind, Detail: e.Detail, OccurredAt: timestamppb.New(e.OccurredAt)})
	}
	return connect.NewResponse(resp), nil
}

// RequestCancel stores the cancel command before anything else, then delivers it to the workflow.
// The answer normally carries the decision; if delivery fails the dispatcher keeps trying.
func (s paymentService) RequestCancel(ctx context.Context, req *connect.Request[corridorv1.RequestCancelRequest]) (*connect.Response[corridorv1.RequestCancelResponse], error) {
	actor := actorFrom(ctx)
	key, err := idempotencyKey(req.Header())
	if err != nil {
		return nil, err
	}
	p, err := s.customerPayment(ctx, req.Msg.PaymentId)
	if err != nil {
		return nil, err
	}
	cmd, err := s.claim(ctx, actor, key, "REQUEST_CANCEL", p.ID.String(), req.Msg, true)
	if err != nil {
		return nil, err
	}
	if cmd, err = s.deliverCommand(ctx, p.ID.String(), cmd); err != nil {
		return nil, unavailable(ctx, err)
	}
	return connect.NewResponse(&corridorv1.RequestCancelResponse{Command: commandMessage(cmd)}), nil
}

func (s paymentService) customerPayment(ctx context.Context, id string) (store.Payment, error) {
	paymentID, err := uuid.Parse(id)
	if err != nil {
		return store.Payment{}, failure(corridorv1.ErrorReason_ERROR_REASON_NOT_FOUND, "payment not found")
	}
	p, err := s.store.CustomerPayment(ctx, paymentID, actorFrom(ctx).ID)
	if errors.Is(err, store.ErrNotFound) {
		return store.Payment{}, failure(corridorv1.ErrorReason_ERROR_REASON_NOT_FOUND, "payment not found")
	}
	if err != nil {
		return store.Payment{}, unavailable(ctx, err)
	}
	return p, nil
}

// claim stores a command under the actor's Idempotency-Key. deliver asks the dispatcher to hand it
// to the payment's workflow; a command settled by the API itself must not.
func (s *Server) claim(ctx context.Context, actor Actor, key, operation, subject string, request any, deliver bool) (store.Command, error) {
	cmd, _, err := s.store.ClaimCommand(ctx, store.CommandClaim{
		ActorKind: actor.Kind, ActorID: actor.ID, RequestKey: key, Operation: operation, SubjectID: subject, Request: request,
	}, deliver)
	if errors.Is(err, store.ErrIdempotencyMismatch) {
		return store.Command{}, failure(corridorv1.ErrorReason_ERROR_REASON_IDEMPOTENCY_MISMATCH, err.Error())
	}
	if err != nil {
		return store.Command{}, unavailable(ctx, err)
	}
	return cmd, nil
}

// deliverCommand gives a received command to its workflow and returns it as the workflow settled
// it. A delivery that fails or times out leaves the command to the dispatcher.
func (s *Server) deliverCommand(ctx context.Context, paymentID string, cmd store.Command) (store.Command, error) {
	if cmd.State != "RECEIVED" || s.temporal == nil {
		return cmd, nil
	}
	deliverCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := dispatch.Deliver(deliverCtx, s.temporal, paymentID, cmd.ID.String()); err != nil {
		slog.WarnContext(ctx, "deliver command", "command", cmd.ID, "error", err)
	}
	return s.store.Command(ctx, cmd.ID)
}

type commandService struct {
	*Server
}

func (s commandService) GetCommand(ctx context.Context, req *connect.Request[corridorv1.GetCommandRequest]) (*connect.Response[corridorv1.GetCommandResponse], error) {
	id, err := uuid.Parse(req.Msg.CommandId)
	if err != nil {
		return nil, failure(corridorv1.ErrorReason_ERROR_REASON_NOT_FOUND, "command not found")
	}
	cmd, err := s.store.Command(ctx, id)
	if errors.Is(err, store.ErrNotFound) || err == nil && cmd.ActorID != actorFrom(ctx).ID {
		return nil, failure(corridorv1.ErrorReason_ERROR_REASON_NOT_FOUND, "command not found")
	}
	if err != nil {
		return nil, unavailable(ctx, err)
	}
	return connect.NewResponse(&corridorv1.GetCommandResponse{Command: commandMessage(cmd)}), nil
}

func between(s string, maxLength int) bool {
	return len(s) > 0 && len(s) <= maxLength
}

func paymentMessage(p store.Payment) *corridorv1.Payment {
	q := p.Quote
	message := &corridorv1.Payment{
		Id:             p.ID.String(),
		Status:         corridorv1.PaymentStatus(corridorv1.PaymentStatus_value["PAYMENT_STATUS_"+p.Status]),
		OutcomeReason:  corridorv1.OutcomeReason(corridorv1.OutcomeReason_value["OUTCOME_REASON_"+p.OutcomeReason]),
		InReview:       p.ReviewOpen,
		ReviewReason:   corridorv1.ReviewReason(corridorv1.ReviewReason_value["REVIEW_REASON_"+p.ReviewReason]),
		QuoteId:        p.QuoteID.String(),
		Sell:           &corridorv1.Money{Currency: q.SellCurrency, Amount: money.Format(q.SellAmount)},
		Fee:            &corridorv1.Money{Currency: q.SellCurrency, Amount: money.Format(q.Fee)},
		Buy:            &corridorv1.Money{Currency: q.BuyCurrency, Amount: money.Format(q.Buy)},
		CustomerRateE8: money.Format(q.CustomerRateE8),
		BeneficiaryId:  p.BeneficiaryID.String(),
		CreatedAt:      timestamppb.New(p.AcceptedAt),
		UpdatedAt:      timestamppb.New(p.UpdatedAt),
		Payout: &corridorv1.Payout{
			Id:               p.Payout.ID.String(),
			Status:           corridorv1.PayoutStatus(corridorv1.PayoutStatus_value["PAYOUT_STATUS_"+p.Payout.Status]),
			Amount:           &corridorv1.Money{Currency: p.Payout.Currency, Amount: money.Format(p.Payout.Amount)},
			ProviderPayoutId: p.Payout.ProviderPayoutID,
		},
	}
	if p.Payout.ReleaseAt != nil {
		message.Payout.ReleaseAt = timestamppb.New(*p.Payout.ReleaseAt)
	}
	if p.Payout.ReturnedAmount > 0 {
		message.Returned = &corridorv1.Money{Currency: p.Payout.Currency, Amount: money.Format(p.Payout.ReturnedAmount)}
	}
	return message
}

func commandMessage(c store.Command) *corridorv1.Command {
	message := &corridorv1.Command{
		Id:         c.ID.String(),
		Operation:  corridorv1.CommandOperation(corridorv1.CommandOperation_value["COMMAND_OPERATION_"+c.Operation]),
		SubjectId:  c.SubjectID,
		State:      corridorv1.CommandState(corridorv1.CommandState_value["COMMAND_STATE_"+c.State]),
		Reason:     c.Reason,
		EffectIds:  c.EffectIDs,
		ReceivedAt: timestamppb.New(c.ReceivedAt),
	}
	if c.AcceptedAt != nil {
		message.AcceptedAt = timestamppb.New(*c.AcceptedAt)
	}
	if c.AppliedAt != nil {
		message.AppliedAt = timestamppb.New(*c.AppliedAt)
	}
	return message
}
