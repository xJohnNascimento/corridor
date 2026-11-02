package payment

import (
	"errors"

	"github.com/google/uuid"
	"go.temporal.io/sdk/temporal"

	"github.com/xJohnNascimento/corridor/internal/ledger"
	"github.com/xJohnNascimento/corridor/internal/money"
	"github.com/xJohnNascimento/corridor/internal/provider"
	"github.com/xJohnNascimento/corridor/internal/store"
)

// providerError keeps transport errors retryable, honours a 429's Retry-After and turns a
// reference conflict into a contract breach, which no retry can fix.
func providerError(err error) error {
	var limited *provider.RateLimitedError
	switch {
	case errors.As(err, &limited):
		return temporal.NewApplicationErrorWithOptions(err.Error(), "RATE_LIMITED", temporal.ApplicationErrorOptions{NextRetryDelay: limited.RetryAfter})
	case errors.Is(err, provider.ErrConflict):
		return temporal.NewNonRetryableApplicationError(err.Error(), ErrorContractBreach, err)
	}
	return err
}

func transitionError(err error) error {
	if errors.Is(err, store.ErrTransitionRefused) {
		return temporal.NewNonRetryableApplicationError(err.Error(), ErrorTransitionConflict, err)
	}
	return err
}

func ledgerError(err error) error {
	if errors.Is(err, ledger.ErrUnexpected) {
		return temporal.NewNonRetryableApplicationError(err.Error(), ErrorLedgerUnexpected, err)
	}
	return err
}

// parseID refuses an id that is not a UUID for good: no retry can make it one.
func parseID(id string) (uuid.UUID, error) {
	parsed, err := uuid.Parse(id)
	if err != nil {
		return uuid.Nil, temporal.NewNonRetryableApplicationError("invalid id "+id, ErrorInvalidArgument, err)
	}
	return parsed, nil
}

// providerAmount reads an amount the provider sent. A missing one is zero, which the callers
// already treat as not matching; one that is not a number is a breach of the provider's contract.
func providerAmount(amount string) (int64, error) {
	if amount == "" {
		return 0, nil
	}
	parsed, err := money.ParseAmount(amount)
	if err != nil {
		return 0, temporal.NewNonRetryableApplicationError(err.Error(), ErrorContractBreach, err)
	}
	return parsed, nil
}
