package ledger

import (
	"context"
	"net/http"

	"connectrpc.com/connect"

	ledgerv1 "github.com/xJohnNascimento/ledger/api/ledger/v1"
	"github.com/xJohnNascimento/ledger/api/ledger/v1/ledgerv1connect"
)

type bearer string

func (b bearer) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		req.Header().Set("Authorization", "Bearer "+string(b))
		return next(ctx, req)
	}
}

func (b bearer) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return func(ctx context.Context, spec connect.Spec) connect.StreamingClientConn {
		conn := next(ctx, spec)
		conn.RequestHeader().Set("Authorization", "Bearer "+string(b))
		return conn
	}
}

func (b bearer) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next
}

func NewService(httpClient *http.Client, url, secret string) ledgerv1connect.LedgerServiceClient {
	return ledgerv1connect.NewLedgerServiceClient(httpClient, url, connect.WithInterceptors(bearer(secret)))
}

type Owner struct {
	Kind ledgerv1.OwnerKind
	ID   string
}

func Customer(id string) Owner { return Owner{Kind: ledgerv1.OwnerKind_OWNER_KIND_CUSTOMER, ID: id} }
func Payment(id string) Owner  { return Owner{Kind: ledgerv1.OwnerKind_OWNER_KIND_PAYMENT, ID: id} }
func House(id string) Owner    { return Owner{Kind: ledgerv1.OwnerKind_OWNER_KIND_HOUSE, ID: id} }
func Provider(id string) Owner { return Owner{Kind: ledgerv1.OwnerKind_OWNER_KIND_PROVIDER, ID: id} }

func EnsureAccount(ctx context.Context, svc ledgerv1connect.LedgerServiceClient, ledgerID string, owner Owner, purpose, asset string) (string, error) {
	resp, err := svc.EnsureAccount(ctx, connect.NewRequest(&ledgerv1.EnsureAccountRequest{
		LedgerId: ledgerID, OwnerKind: owner.Kind, OwnerId: owner.ID, Purpose: purpose, AssetCode: asset,
	}))
	if err != nil {
		return "", err
	}
	return resp.Msg.Account.Id, nil
}
