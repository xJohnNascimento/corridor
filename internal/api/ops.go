package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/timestamppb"

	corridorv1 "github.com/xJohnNascimento/corridor/api/corridor/v1"
	"github.com/xJohnNascimento/corridor/api/corridor/v1/corridorv1connect"
	"github.com/xJohnNascimento/corridor/internal/money"
	"github.com/xJohnNascimento/corridor/internal/store"
)

type opsService struct {
	corridorv1connect.UnimplementedOpsServiceHandler
	*Server
}

func operator(ctx context.Context) (Actor, error) {
	actor := actorFrom(ctx)
	if actor.Kind != ActorOperator {
		return Actor{}, failure(corridorv1.ErrorReason_ERROR_REASON_PERMISSION_DENIED, "an operator key is required")
	}
	return actor, nil
}

func (s opsService) ListReviews(ctx context.Context, req *connect.Request[corridorv1.ListReviewsRequest]) (*connect.Response[corridorv1.ListReviewsResponse], error) {
	if _, err := operator(ctx); err != nil {
		return nil, err
	}
	limit := int(req.Msg.PageSize)
	if limit <= 0 || limit > 200 {
		limit = 200
	}
	reviews, err := s.store.Reviews(ctx, limit)
	if err != nil {
		return nil, unavailable(ctx, err)
	}
	resp := &corridorv1.ListReviewsResponse{}
	for _, r := range reviews {
		resp.Reviews = append(resp.Reviews, &corridorv1.Review{
			PaymentId: r.PaymentID.String(), Reason: corridorv1.ReviewReason(corridorv1.ReviewReason_value["REVIEW_REASON_"+r.Reason]),
			Revision: r.Revision, OpenedAt: timestamppb.New(r.OpenedAt), CurrentProposalId: r.CurrentProposalID,
		})
	}
	return connect.NewResponse(resp), nil
}

type proposalRequest struct {
	PaymentID         string `json:"payment_id"`
	SubjectKind       string `json:"subject_kind"`
	SubjectID         string `json:"subject_id"`
	ReviewRevision    int32  `json:"review_revision"`
	Decision          string `json:"decision"`
	ProviderReference string `json:"provider_reference"`
	Currency          string `json:"currency"`
	Amount            string `json:"amount"`
	EvidenceID        string `json:"evidence_id"`
	EvidenceDigest    string `json:"evidence_digest"`
	Note              string `json:"note"`
}

// ProposeResolution records a proposal bound to its evidence. Every attempt is a command: a
// proposal that breaks a rule is stored as REJECTED with its reason.
func (s opsService) ProposeResolution(ctx context.Context, req *connect.Request[corridorv1.ProposeResolutionRequest]) (*connect.Response[corridorv1.ProposeResolutionResponse], error) {
	actor, err := operator(ctx)
	if err != nil {
		return nil, err
	}
	key, err := idempotencyKey(req.Header())
	if err != nil {
		return nil, err
	}
	m := req.Msg
	if digest := m.Evidence.GetDigest(); digest != "" && !isDigest(digest) {
		return nil, failure(corridorv1.ErrorReason_ERROR_REASON_VALIDATION_FAILED, "an evidence digest is 64 hexadecimal characters")
	}
	request := proposalRequest{
		PaymentID: m.PaymentId, SubjectKind: strings.TrimPrefix(m.SubjectKind.String(), "SUBJECT_KIND_"), SubjectID: m.SubjectId,
		ReviewRevision: m.ReviewRevision, Decision: strings.TrimPrefix(m.Decision.String(), "DECISION_"), ProviderReference: m.ProviderReference,
		Currency: m.Amount.GetCurrency(), Amount: m.Amount.GetAmount(), EvidenceID: m.Evidence.GetId(), EvidenceDigest: m.Evidence.GetDigest(), Note: m.Note,
	}
	cmd, err := s.claim(ctx, actor, key, "PROPOSE_RESOLUTION", m.PaymentId, request, false)
	if err != nil {
		return nil, err
	}
	resp := &corridorv1.ProposeResolutionResponse{}
	if cmd.State == "RECEIVED" {
		var refused *store.RefusedError
		if err := s.propose(ctx, cmd.ID, actor, request); err != nil && !errors.As(err, &refused) {
			return nil, unavailable(ctx, err)
		}
		if cmd, err = s.store.Command(ctx, cmd.ID); err != nil {
			return nil, unavailable(ctx, err)
		}
	}
	if len(cmd.EffectIDs) == 1 {
		proposal, err := uuid.Parse(cmd.EffectIDs[0])
		if err != nil {
			return nil, unavailable(ctx, err)
		}
		resp.ProposalId = cmd.EffectIDs[0]
		if resp.ProposalDigest, err = s.store.ProposalDigest(ctx, proposal); err != nil {
			return nil, unavailable(ctx, err)
		}
	}
	resp.Command = commandMessage(cmd)
	return connect.NewResponse(resp), nil
}

// propose settles the command itself: with the proposal's id in the transaction that stores it, or
// as REJECTED with the refusal's reason.
func (s opsService) propose(ctx context.Context, command uuid.UUID, actor Actor, r proposalRequest) error {
	payment, err := uuid.Parse(r.PaymentID)
	if err != nil {
		return s.reject(ctx, command, "unknown_payment")
	}
	var amount int64
	if r.Amount != "" {
		if amount, err = money.ParseAmount(r.Amount); err != nil {
			return s.reject(ctx, command, "invalid_amount")
		}
	}
	return s.store.Propose(ctx, command, store.Proposal{
		PaymentID: payment, SubjectKind: r.SubjectKind, SubjectID: r.SubjectID, ReviewRevision: int(r.ReviewRevision),
		Decision: r.Decision, ProviderReference: r.ProviderReference, Currency: r.Currency, Amount: amount,
		EvidenceID: r.EvidenceID, EvidenceDigest: r.EvidenceDigest, Maker: actor.ID,
	})
}

func (s opsService) reject(ctx context.Context, command uuid.UUID, reason string) error {
	if err := s.store.RejectCommand(ctx, command, reason); err != nil {
		return err
	}
	return &store.RefusedError{Reason: reason}
}

func isDigest(s string) bool {
	raw, err := hex.DecodeString(s)
	return err == nil && len(raw) == sha256.Size
}

type approvalRequest struct {
	ProposalID     string `json:"proposal_id"`
	ProposalDigest string `json:"proposal_digest"`
}

// ApproveResolution stores the approval as a command for the payment and delivers it to the
// workflow, whose guarded decision checks the proposal, its digest and the second operator.
func (s opsService) ApproveResolution(ctx context.Context, req *connect.Request[corridorv1.ApproveResolutionRequest]) (*connect.Response[corridorv1.ApproveResolutionResponse], error) {
	actor, err := operator(ctx)
	if err != nil {
		return nil, err
	}
	key, err := idempotencyKey(req.Header())
	if err != nil {
		return nil, err
	}
	proposal, err := uuid.Parse(req.Msg.ProposalId)
	if err != nil {
		return nil, failure(corridorv1.ErrorReason_ERROR_REASON_NOT_FOUND, "proposal not found")
	}
	payment, err := s.store.ProposalPayment(ctx, proposal)
	if errors.Is(err, store.ErrNotFound) {
		return nil, failure(corridorv1.ErrorReason_ERROR_REASON_NOT_FOUND, "proposal not found")
	}
	if err != nil {
		return nil, unavailable(ctx, err)
	}
	cmd, err := s.claim(ctx, actor, key, "APPROVE_RESOLUTION", payment.String(), approvalRequest{ProposalID: req.Msg.ProposalId, ProposalDigest: req.Msg.ProposalDigest}, true)
	if err != nil {
		return nil, err
	}
	if cmd, err = s.deliverCommand(ctx, payment.String(), cmd); err != nil {
		return nil, unavailable(ctx, err)
	}
	return connect.NewResponse(&corridorv1.ApproveResolutionResponse{Command: commandMessage(cmd)}), nil
}
