-- +goose Up

set local role fx_owner;

alter table fx.payment add constraint payment_review_reason_check check (
    review_reason is null or review_reason in (
        'FILL_OUT_OF_TOLERANCE', 'PAYOUT_UNCONFIRMED', 'CONFLICTING_PROVIDER_EVIDENCE',
        'PROVIDER_CONTRACT_BREACH', 'LEDGER_UNEXPECTED', 'RECOVERY_REQUIRED'
    )
);

reset role;
