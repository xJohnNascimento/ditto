-- +goose Up

create table provider_account (
    id text primary key check (id ~ '^[a-z][a-z0-9-]{0,62}$')
);

create table rate (
    sell_currency text not null check (sell_currency ~ '^[A-Z]{3}$'),
    buy_currency text not null check (buy_currency ~ '^[A-Z]{3}$'),
    rate_e8 bigint not null check (rate_e8 > 0),
    updated_at timestamptz not null default now(),
    primary key (sell_currency, buy_currency),
    check (sell_currency <> buy_currency)
);

create table quote (
    id uuid primary key,
    provider_account_id text not null references provider_account,
    client_reference text not null check (length(client_reference) between 1 and 128),
    request_hash bytea not null check (length(request_hash) = 32),
    sell_currency text not null,
    buy_currency text not null,
    sell_amount bigint not null check (sell_amount > 0),
    rate_e8 bigint not null check (rate_e8 > 0),
    buy_amount bigint not null check (buy_amount > 0),
    created_at timestamptz not null default now(),
    expires_at timestamptz not null,
    unique (provider_account_id, client_reference),
    unique (id, provider_account_id)
);

-- One row per reference serializes create, close, cancel and scheduled work. A fence written before
-- any create leaves request_hash null; the state is then a tombstone that refuses execution.
create table operation_reference (
    provider_account_id text not null references provider_account,
    kind text not null check (kind in ('TRADE', 'PAYOUT')),
    reference text not null check (length(reference) between 1 and 128),
    request_hash bytea check (length(request_hash) = 32),
    state text not null check (state in (
        'FILLED', 'REJECTED', 'CLOSED', 'ACCEPTED', 'PROCESSING', 'COMPLETED', 'FAILED', 'CANCELLED'
    )),
    version integer not null default 1 check (version > 0),
    created_at timestamptz not null default now(),
    updated_at timestamptz not null default now(),
    primary key (provider_account_id, kind, reference),
    check (
        (kind = 'TRADE' and state in ('FILLED', 'REJECTED', 'CLOSED'))
        or (kind = 'PAYOUT' and state in ('ACCEPTED', 'PROCESSING', 'COMPLETED', 'FAILED', 'CANCELLED', 'REJECTED'))
    )
);

create table cash_account (
    provider_account_id text not null references provider_account,
    currency text not null check (currency ~ '^[A-Z]{3}$'),
    balance bigint not null default 0 check (balance >= 0),
    last_sequence bigint not null default 0 check (last_sequence >= 0),
    primary key (provider_account_id, currency)
);

create table cash_movement (
    provider_account_id text not null,
    currency text not null,
    sequence bigint not null check (sequence > 0),
    amount bigint not null check (amount <> 0),
    balance_after bigint not null check (balance_after >= 0),
    kind text not null check (kind in ('FUNDING', 'TRADE_SELL', 'TRADE_BUY', 'PAYOUT_DEBIT', 'PAYOUT_REFUND')),
    reference text not null,
    effect_id uuid not null,
    leg smallint not null check (leg > 0),
    created_at timestamptz not null default now(),
    primary key (provider_account_id, currency, sequence),
    unique (effect_id, leg),
    foreign key (provider_account_id, currency) references cash_account
);

create table request_budget (
    provider_account_id text primary key references provider_account,
    rate_per_second numeric not null check (rate_per_second > 0),
    burst integer not null check (burst > 0),
    tokens numeric not null check (tokens >= 0),
    updated_at timestamptz not null,
    throttled_until timestamptz
);

create table request_observation (
    id bigint generated always as identity primary key,
    provider_account_id text not null references provider_account,
    route text not null,
    reference text,
    status integer not null,
    throttled boolean not null,
    fault text,
    received_at timestamptz not null
);

create index request_observation_received on request_observation (received_at);
