package api

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"

	"connectrpc.com/connect"

	corridorv1 "github.com/xJohnNascimento/corridor/api/corridor/v1"
	"github.com/xJohnNascimento/corridor/internal/store"
)

const (
	ActorCustomer = "CUSTOMER"
	ActorOperator = "OPERATOR"
	ActorAuditor  = "AUDITOR"
)

type Actor struct {
	Kind string
	ID   string
}

type actorKey struct{}

func actorFrom(ctx context.Context) Actor {
	return ctx.Value(actorKey{}).(Actor)
}

// authenticator resolves a bearer key to an actor. The customer port accepts customer keys only and
// the internal port operator and auditor keys only, so a customer key never reaches operations.
type authenticator struct {
	resolve func(ctx context.Context, keyHash []byte) (Actor, error)
}

func (a authenticator) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		actor, err := a.authenticate(ctx, req.Header())
		if err != nil {
			return nil, err
		}
		return next(context.WithValue(ctx, actorKey{}, actor), req)
	}
}

func (a authenticator) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (a authenticator) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		actor, err := a.authenticate(ctx, conn.RequestHeader())
		if err != nil {
			return err
		}
		return next(context.WithValue(ctx, actorKey{}, actor), conn)
	}
}

func (a authenticator) authenticate(ctx context.Context, header http.Header) (Actor, error) {
	key, ok := strings.CutPrefix(header.Get("Authorization"), "Bearer ")
	if !ok || key == "" {
		return Actor{}, failure(corridorv1.ErrorReason_ERROR_REASON_UNAUTHENTICATED, "a bearer key is required")
	}
	actor, err := a.resolve(ctx, store.KeyHash(key))
	if errors.Is(err, store.ErrNotFound) {
		return Actor{}, failure(corridorv1.ErrorReason_ERROR_REASON_UNAUTHENTICATED, "unknown key")
	}
	if err != nil {
		return Actor{}, unavailable(ctx, err)
	}
	return actor, nil
}

func customerKeys(s *store.Store) authenticator {
	return authenticator{resolve: func(ctx context.Context, keyHash []byte) (Actor, error) {
		id, err := s.CustomerByKey(ctx, keyHash)
		return Actor{Kind: ActorCustomer, ID: id}, err
	}}
}

func internalKeys(s *store.Store, auditorKeyHash []byte) authenticator {
	return authenticator{resolve: func(ctx context.Context, keyHash []byte) (Actor, error) {
		if auditorKeyHash != nil && subtle.ConstantTimeCompare(keyHash, auditorKeyHash) == 1 {
			return Actor{Kind: ActorAuditor, ID: "auditor"}, nil
		}
		id, err := s.OperatorByKey(ctx, keyHash)
		return Actor{Kind: ActorOperator, ID: id}, err
	}}
}

func idempotencyKey(header http.Header) (string, error) {
	key := header.Get("Idempotency-Key")
	if key == "" || len(key) > 128 {
		return "", failure(corridorv1.ErrorReason_ERROR_REASON_VALIDATION_FAILED, "an Idempotency-Key header of 1 to 128 characters is required")
	}
	return key, nil
}
