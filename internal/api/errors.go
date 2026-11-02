package api

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"time"

	"connectrpc.com/connect"

	corridorv1 "github.com/xJohnNascimento/corridor/api/corridor/v1"
)

var codeByReason = map[corridorv1.ErrorReason]connect.Code{
	corridorv1.ErrorReason_ERROR_REASON_VALIDATION_FAILED:    connect.CodeInvalidArgument,
	corridorv1.ErrorReason_ERROR_REASON_IDEMPOTENCY_MISMATCH: connect.CodeInvalidArgument,
	corridorv1.ErrorReason_ERROR_REASON_AMOUNT_OUT_OF_RANGE:  connect.CodeFailedPrecondition,
	corridorv1.ErrorReason_ERROR_REASON_CORRIDOR_DISABLED:    connect.CodeFailedPrecondition,
	corridorv1.ErrorReason_ERROR_REASON_QUOTE_EXPIRED:        connect.CodeFailedPrecondition,
	corridorv1.ErrorReason_ERROR_REASON_QUOTE_USED:           connect.CodeAlreadyExists,
	corridorv1.ErrorReason_ERROR_REASON_NOT_FOUND:            connect.CodeNotFound,
	corridorv1.ErrorReason_ERROR_REASON_UNAUTHENTICATED:      connect.CodeUnauthenticated,
	corridorv1.ErrorReason_ERROR_REASON_PERMISSION_DENIED:    connect.CodePermissionDenied,
	corridorv1.ErrorReason_ERROR_REASON_RATE_LIMITED:         connect.CodeResourceExhausted,
	corridorv1.ErrorReason_ERROR_REASON_UNAVAILABLE:          connect.CodeUnavailable,
}

func failure(reason corridorv1.ErrorReason, message string) *connect.Error {
	return failureWith(reason, message, &corridorv1.ErrorDetail{})
}

func failureWith(reason corridorv1.ErrorReason, message string, detail *corridorv1.ErrorDetail) *connect.Error {
	err := connect.NewError(codeByReason[reason], errors.New(message))
	detail.Reason = reason
	if d, detailErr := connect.NewErrorDetail(detail); detailErr == nil {
		err.AddDetail(d)
	}
	return err
}

func rateLimited(retryAfter time.Duration) *connect.Error {
	return failureWith(corridorv1.ErrorReason_ERROR_REASON_RATE_LIMITED, "the quote budget is spent",
		&corridorv1.ErrorDetail{RetryAfterSeconds: wholeSeconds(retryAfter)})
}

func wholeSeconds(d time.Duration) int32 {
	seconds := d / time.Second
	if seconds < 0 {
		return 0
	}
	if seconds > math.MaxInt32 {
		return math.MaxInt32
	}
	return int32(seconds)
}

// unavailable hides an infrastructure error from the client, who retries with the same key.
func unavailable(ctx context.Context, err error) *connect.Error {
	slog.ErrorContext(ctx, "request failed", "error", err)
	return failure(corridorv1.ErrorReason_ERROR_REASON_UNAVAILABLE, "temporarily unavailable; retry with the same Idempotency-Key")
}
