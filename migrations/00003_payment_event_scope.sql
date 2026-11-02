-- +goose Up

set local role fx_owner;

-- Transition events use keys such as payment/converted, which repeat across payments, so a key
-- is unique within its payment only.
alter table fx.payment_event drop constraint payment_event_effect_key_key;
alter table fx.payment_event add constraint payment_event_effect_key unique (payment_id, effect_key);

reset role;
