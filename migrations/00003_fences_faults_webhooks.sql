-- +goose Up

create table scenario_arm (
    id uuid primary key,
    fault text not null,
    target_kind text not null check (target_kind in ('quote', 'payout', 'account')),
    target_value text not null check (length(target_value) between 1 and 128),
    args jsonb not null default '{}',
    remaining_uses integer not null check (remaining_uses >= 0),
    created_at timestamptz not null default now()
);

create index scenario_arm_target on scenario_arm (target_kind, target_value) where remaining_uses > 0;

-- A consumption commits in the same transaction as the effect it changed, so it is the server's
-- proof that the fault actually fired.
create table scenario_consumption (
    arm_id uuid not null references scenario_arm,
    request_id text not null,
    consumed_at timestamptz not null default now(),
    primary key (arm_id, request_id)
);

create table webhook_event (
    id text primary key,
    provider_account_id text not null references provider_account,
    reference text not null,
    status text not null,
    sequence integer not null check (sequence between 1 and 3),
    body text not null,
    created_at timestamptz not null default now()
);

create table webhook_delivery (
    event_id text not null references webhook_event,
    copy smallint not null check (copy > 0),
    attempts integer not null default 0 check (attempts >= 0),
    next_attempt_at timestamptz not null,
    lease_until timestamptz,
    delivered_at timestamptz,
    gave_up_at timestamptz,
    last_error text,
    primary key (event_id, copy)
);

create index webhook_delivery_due on webhook_delivery (next_attempt_at) where delivered_at is null and gave_up_at is null;

alter table request_observation add column request_id text;
