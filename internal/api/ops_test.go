package api_test

import (
	"context"
	"strings"
	"testing"

	"connectrpc.com/connect"

	corridorv1 "github.com/xJohnNascimento/corridor/api/corridor/v1"
	"github.com/xJohnNascimento/corridor/api/corridor/v1/corridorv1connect"
)

func (h *harness) ops(key, idempotency string) corridorv1connect.OpsServiceClient {
	return corridorv1connect.NewOpsServiceClient(h.internal.Client(), h.internal.URL, connect.WithInterceptors(credentials{key, idempotency}))
}

func TestAKeyOfTheWrongKindIsDeniedNotUnauthenticated(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()

	_, err := h.ops("auditor-key", "").ListReviews(ctx, connect.NewRequest(&corridorv1.ListReviewsRequest{}))
	if reasonOf(err) != corridorv1.ErrorReason_ERROR_REASON_PERMISSION_DENIED || connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("an auditor key on an operator call: %v", err)
	}

	stream, err := h.ops("alice-key", "").ExportSnapshot(ctx, connect.NewRequest(&corridorv1.ExportSnapshotRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if stream.Receive() || reasonOf(stream.Err()) != corridorv1.ErrorReason_ERROR_REASON_PERMISSION_DENIED {
		t.Fatalf("an operator key on the export: %v", stream.Err())
	}

	stream, err = h.ops("nobody-key", "").ExportSnapshot(ctx, connect.NewRequest(&corridorv1.ExportSnapshotRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if stream.Receive() || reasonOf(stream.Err()) != corridorv1.ErrorReason_ERROR_REASON_UNAUTHENTICATED {
		t.Fatalf("an unknown key on the export: %v", stream.Err())
	}
}

func TestTheExportIsUnavailableWithoutAnAuditStore(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	stream, err := h.ops("auditor-key", "").ExportSnapshot(context.Background(), connect.NewRequest(&corridorv1.ExportSnapshotRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if stream.Receive() || connect.CodeOf(stream.Err()) != connect.CodeUnavailable {
		t.Fatalf("an auditor key without an audit store: %v", stream.Err())
	}
}

func TestAProposalThatCannotBeRightIsRefusedAtTheBoundaryOrRecordedAsRejected(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	propose := func(key string, request *corridorv1.ProposeResolutionRequest) (*corridorv1.ProposeResolutionResponse, error) {
		resp, err := h.ops("alice-key", key).ProposeResolution(ctx, connect.NewRequest(request))
		if err != nil {
			return nil, err
		}
		return resp.Msg, nil
	}

	_, err := propose("bad-digest", &corridorv1.ProposeResolutionRequest{
		PaymentId: "00000000-0000-0000-0000-000000000000", Evidence: &corridorv1.Evidence{Id: "e-1", Digest: strings.Repeat("z", 64)},
	})
	if reasonOf(err) != corridorv1.ErrorReason_ERROR_REASON_VALIDATION_FAILED {
		t.Fatalf("an evidence digest that is not hex: %v", err)
	}

	for _, attempt := range []string{"first", "retry"} {
		resp, err := propose("unknown-payment", &corridorv1.ProposeResolutionRequest{PaymentId: "00000000-0000-0000-0000-000000000000"})
		if err != nil || resp.Command.State != corridorv1.CommandState_COMMAND_STATE_REJECTED || resp.Command.Reason != "unknown_payment" || resp.ProposalId != "" {
			t.Fatalf("%s: %+v, %v", attempt, resp, err)
		}
	}
}
