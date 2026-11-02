-- +goose Up

set local role fx_owner;

-- An approval is decided inside the workflow from durable state, so the command keeps its request
-- body next to the hash it was deduplicated by.
alter table fx.product_command add column request jsonb;

reset role;
