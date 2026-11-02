-- +goose Up

-- +goose StatementBegin
do $$
begin
    if not exists (select from pg_roles where rolname = 'fx_owner') then
        create role fx_owner nologin;
    end if;
    if not exists (select from pg_roles where rolname = 'fx_app') then
        create role fx_app login;
    end if;
    if not exists (select from pg_roles where rolname = 'fx_audit') then
        create role fx_audit login;
    end if;
end
$$;
-- +goose StatementEnd

grant fx_owner to current_user;
create schema fx authorization fx_owner;
set local role fx_owner;

alter default privileges for role fx_owner revoke execute on functions from public;
alter default privileges for role fx_owner in schema fx grant select, insert, update on tables to fx_app;
alter default privileges for role fx_owner in schema fx grant select on tables to fx_audit;
alter default privileges for role fx_owner in schema fx grant usage on sequences to fx_app;
grant usage on schema fx to fx_app, fx_audit;

create table fx.customer (
    id text primary key check (id ~ '^[a-z0-9][a-z0-9-]{0,62}$'),
    name text not null check (length(name) between 1 and 140),
    status text not null default 'ACTIVE' check (status in ('ACTIVE', 'DISABLED')),
    key_hash bytea not null unique check (length(key_hash) = 32),
    created_at timestamptz not null default now()
);

create table fx.customer_account (
    customer_id text not null references fx.customer,
    currency text not null check (currency ~ '^[A-Z]{3}$'),
    ledger_account_id uuid not null unique,
    primary key (customer_id, currency)
);

create table fx.operator (
    id text primary key check (id ~ '^[a-z][a-z0-9-]{0,62}$'),
    name text not null,
    key_hash bytea not null unique check (length(key_hash) = 32)
);

create table fx.rate_bucket (
    customer_id text primary key references fx.customer,
    tokens numeric not null check (tokens >= 0),
    updated_at timestamptz not null
);

create table fx.product_command (
    id uuid primary key,
    actor_kind text not null check (actor_kind in ('CUSTOMER', 'OPERATOR', 'SYSTEM')),
    actor_id text not null,
    request_key text not null check (length(request_key) between 1 and 128),
    operation text not null check (operation in (
        'CREATE_QUOTE', 'REQUEST_CANCEL', 'PROPOSE_RESOLUTION', 'APPROVE_RESOLUTION', 'ACKNOWLEDGE_FINDING'
    )),
    subject_id text,
    request_hash bytea not null check (length(request_hash) = 32),
    state text not null default 'RECEIVED' check (state in ('RECEIVED', 'ACCEPTED', 'APPLIED', 'REJECTED')),
    reason text,
    effect_ids text[] not null default '{}',
    delivery_pending boolean not null default false,
    lease_until timestamptz,
    received_at timestamptz not null default now(),
    accepted_at timestamptz,
    applied_at timestamptz,
    unique (actor_id, request_key)
);

create index product_command_pending on fx.product_command (received_at) where delivery_pending;

create table fx.quote (
    id uuid primary key,
    customer_id text not null references fx.customer,
    command_id uuid not null unique references fx.product_command,
    sell_currency text not null,
    buy_currency text not null,
    sell_amount bigint not null check (sell_amount > 0),
    fee bigint not null check (fee >= 0),
    net_sell bigint not null check (net_sell > 0),
    provider_rate_e8 bigint not null check (provider_rate_e8 > 0),
    customer_rate_e8 bigint not null check (customer_rate_e8 > 0),
    provider_buy bigint not null check (provider_buy > 0),
    buy bigint not null check (buy > 0),
    spread bigint not null,
    policy_version text not null,
    provider_quote_id text not null,
    provider_expires_at timestamptz not null,
    expires_at timestamptz not null,
    created_at timestamptz not null default now(),
    unique (id, customer_id),
    check (sell_currency <> buy_currency),
    check (net_sell = sell_amount - fee),
    check (spread = provider_buy - buy and spread >= 0),
    check (provider_expires_at >= expires_at)
);

create table fx.beneficiary (
    id uuid primary key,
    customer_id text not null references fx.customer,
    name text not null check (length(name) between 1 and 140),
    account text not null check (length(account) between 1 and 34),
    bank_code text not null check (length(bank_code) between 1 and 11),
    country text not null check (country ~ '^[A-Z]{2}$'),
    details_hash bytea not null,
    unique (customer_id, details_hash)
);

create table fx.payment (
    id uuid primary key,
    customer_id text not null references fx.customer,
    request_key text not null,
    request_hash bytea not null check (length(request_hash) = 32),
    quote_id uuid not null unique,
    beneficiary_id uuid not null references fx.beneficiary,
    payout_id uuid not null unique,
    remittance_info text check (length(remittance_info) <= 140),
    status text not null check (status in (
        'ADMITTED', 'CONVERTING', 'CONVERTED', 'PAYING_OUT', 'COMPLETED', 'REJECTED', 'EXPIRED', 'PAYOUT_FAILED', 'PAYOUT_CANCELLED'
    )),
    outcome_reason text check (outcome_reason in (
        'INSUFFICIENT_FUNDS', 'LIQUIDITY_UNAVAILABLE', 'RATE_UNAVAILABLE', 'CUSTOMER_CANCELLED',
        'BENEFICIARY_ACCOUNT_CLOSED', 'PAYOUT_REJECTED', 'RESOLVED_NOT_PAID'
    )),
    version integer not null default 1,
    last_transition_key text,
    review_open boolean not null default false,
    review_reason text,
    review_revision integer not null default 0,
    review_opened_at timestamptz,
    policy jsonb not null,
    workflow_start_pending boolean not null default true,
    start_lease_until timestamptz,
    accepted_at timestamptz not null default now(),
    updated_at timestamptz not null default now(),
    unique (customer_id, request_key),
    foreign key (quote_id, customer_id) references fx.quote (id, customer_id),
    check ((outcome_reason is null) = (status not in ('REJECTED', 'EXPIRED', 'PAYOUT_FAILED', 'PAYOUT_CANCELLED'))),
    check (review_open = (review_reason is not null))
);

create index payment_customer on fx.payment (customer_id, accepted_at desc);
create index payment_start_pending on fx.payment (accepted_at) where workflow_start_pending;
create index payment_review on fx.payment (review_opened_at) where review_open;

create table fx.trade (
    payment_id uuid primary key references fx.payment,
    reference text not null unique,
    sell_currency text not null,
    buy_currency text not null,
    sell_amount bigint not null check (sell_amount > 0),
    quoted_buy bigint not null check (quoted_buy > 0),
    provider_quote_id text not null,
    status text not null default 'PENDING' check (status in ('PENDING', 'FILLED', 'REJECTED', 'CLOSED')),
    submission_started_at timestamptz,
    provider_trade_id text,
    delivered_amount bigint check (delivered_amount > 0),
    rate_e8 bigint,
    evidence_id uuid,
    reservation_id uuid,
    entry_id uuid,
    check (status <> 'FILLED' or (provider_trade_id is not null and delivered_amount is not null and evidence_id is not null))
);

create table fx.provider_evidence (
    id uuid primary key,
    source text not null check (source in ('READ', 'HINT', 'STATEMENT', 'FENCE')),
    operation text not null check (operation in ('TRADE', 'PAYOUT')),
    reference text not null,
    outcome text not null,
    currency text,
    amount bigint,
    sequence integer,
    payload jsonb not null,
    digest bytea not null check (length(digest) = 32),
    source_identity text not null unique,
    recorded_at timestamptz not null default now()
);

create index provider_evidence_reference on fx.provider_evidence (operation, reference);

create table fx.payout (
    id uuid primary key,
    payment_id uuid not null unique references fx.payment,
    amount bigint not null check (amount > 0),
    currency text not null,
    beneficiary_id uuid not null references fx.beneficiary,
    clearing_account_id uuid,
    attempt integer not null default 1 check (attempt > 0),
    reservation_id uuid,
    release_at timestamptz,
    cancel_accepted_at timestamptz,
    cancel_command_id uuid references fx.product_command,
    submission_started_at timestamptz,
    provider_payout_id text,
    status text not null default 'PLANNED' check (status in (
        'PLANNED', 'RESERVING', 'WAITING_FUNDS', 'WAITING_RELEASE', 'CANCELLING', 'SUBMITTING', 'SENT',
        'COMPLETED', 'FAILED', 'CANCELLED', 'SKIPPED'
    )),
    version integer not null default 1,
    last_transition_key text,
    evidence_id uuid references fx.provider_evidence,
    entry_id uuid,
    returned_amount bigint,
    created_at timestamptz not null default now(),
    updated_at timestamptz not null default now(),
    check (status <> 'COMPLETED' or evidence_id is not null),
    check (status <> 'FAILED' or evidence_id is not null),
    check (status <> 'CANCELLED' or (cancel_accepted_at is not null and submission_started_at is null)),
    check (status not in ('SUBMITTING', 'SENT') or cancel_accepted_at is null)
);

create table fx.payment_event (
    id bigint generated always as identity primary key,
    payment_id uuid not null references fx.payment,
    kind text not null,
    detail text not null default '',
    visibility text not null check (visibility in ('CUSTOMER', 'OPERATOR')),
    effect_key text not null unique,
    occurred_at timestamptz not null default now()
);

create index payment_event_payment on fx.payment_event (payment_id, id);

create table fx.webhook_inbox (
    provider text not null,
    event_id text not null,
    payload_hash bytea not null,
    payload jsonb not null,
    reference text,
    status text not null default 'PENDING' check (status in ('PENDING', 'SIGNALLED', 'NO_TARGET', 'CONFLICT')),
    lease_until timestamptz,
    received_at timestamptz not null default now(),
    primary key (provider, event_id)
);

create index webhook_inbox_pending on fx.webhook_inbox (received_at) where status = 'PENDING';

create table fx.ledger_intent (
    namespace text not null,
    reference text not null,
    operation text not null,
    payment_id uuid not null references fx.payment,
    request jsonb not null,
    request_hash bytea not null,
    observed jsonb,
    created_at timestamptz not null default now(),
    observed_at timestamptz,
    primary key (namespace, reference)
);

create index ledger_intent_unresolved on fx.ledger_intent (created_at) where observed is null;

create table fx.resolution_proposal (
    id uuid primary key,
    payment_id uuid not null references fx.payment,
    review_reason text not null,
    review_revision integer not null,
    decision text not null check (decision in ('ACCEPT_FILL', 'PAID', 'NOT_PAID', 'RETRY_STEP', 'RESUME')),
    subject_kind text not null check (subject_kind in ('TRADE', 'PAYOUT', 'PAYMENT')),
    subject_id text not null,
    provider_reference text not null,
    currency text,
    amount bigint,
    evidence_id uuid references fx.provider_evidence,
    evidence_digest bytea,
    digest bytea not null unique,
    maker text not null references fx.operator,
    proposal_revision integer not null,
    superseded_by uuid references fx.resolution_proposal,
    approver text references fx.operator,
    approval_command_id uuid references fx.product_command,
    created_at timestamptz not null default now(),
    approved_at timestamptz,
    unique (payment_id, review_revision, proposal_revision),
    check (approver is null or approver <> maker)
);

create unique index resolution_proposal_approved on fx.resolution_proposal (payment_id, review_revision) where approver is not null;

create table fx.failpoint_arm (
    name text primary key check (name in (
        'admission.afterCommit', 'ledger.afterCommit.dropReply', 'ledger.afterCommit.holdReply',
        'payout.beforeReserve.hold', 'command.afterAccepted'
    )),
    subject text not null default '',
    remaining integer not null check (remaining >= 0),
    held boolean not null default false,
    armed_at timestamptz not null default now()
);

-- +goose StatementBegin
create function fx.refuse_terminal_change() returns trigger
language plpgsql
set search_path = fx, pg_temp
as $$
begin
    if old.status = any(tg_argv) and new.status is distinct from old.status then
        raise exception '% % is terminal (%)', tg_table_name, old.id, old.status using errcode = 'FX001';
    end if;
    return new;
end
$$;
-- +goose StatementEnd

create trigger payment_terminal before update on fx.payment
    for each row execute function fx.refuse_terminal_change('COMPLETED', 'REJECTED', 'EXPIRED', 'PAYOUT_FAILED', 'PAYOUT_CANCELLED');
create trigger payout_terminal before update on fx.payout
    for each row execute function fx.refuse_terminal_change('COMPLETED', 'FAILED', 'CANCELLED', 'SKIPPED');

-- +goose StatementBegin
create function fx.refuse_change() returns trigger
language plpgsql
set search_path = fx, pg_temp
as $$
begin
    raise exception '% on fx.% is not allowed', tg_op, tg_table_name using errcode = 'FX002';
end
$$;
-- +goose StatementEnd

create trigger payment_event_append_only before update or delete on fx.payment_event
    for each row execute function fx.refuse_change();
create trigger provider_evidence_append_only before update or delete on fx.provider_evidence
    for each row execute function fx.refuse_change();

reset role;
