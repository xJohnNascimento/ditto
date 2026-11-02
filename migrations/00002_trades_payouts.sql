-- +goose Up

-- PENDING exists only inside the transaction that inserted the reference; the same transaction
-- always replaces it with the outcome before committing.
alter table operation_reference drop constraint operation_reference_check;
alter table operation_reference drop constraint operation_reference_state_check;
alter table operation_reference add check (
    state = 'PENDING'
    or (kind = 'TRADE' and state in ('FILLED', 'REJECTED', 'CLOSED'))
    or (kind = 'PAYOUT' and state in ('ACCEPTED', 'PROCESSING', 'COMPLETED', 'FAILED', 'CANCELLED', 'REJECTED'))
);

create table trade (
    id uuid primary key,
    provider_account_id text not null,
    kind text not null default 'TRADE' check (kind = 'TRADE'),
    reference text not null,
    quote_id uuid not null,
    sell_currency text not null,
    buy_currency text not null,
    sell_amount bigint not null check (sell_amount > 0),
    buy_amount bigint not null check (buy_amount > 0),
    rate_e8 bigint not null check (rate_e8 > 0),
    reason text check (reason in ('LAST_LOOK', 'QUOTE_EXPIRED', 'INSUFFICIENT_BALANCE')),
    executed_at timestamptz not null default now(),
    unique (provider_account_id, reference),
    foreign key (provider_account_id, kind, reference) references operation_reference,
    foreign key (quote_id, provider_account_id) references quote (id, provider_account_id)
);

create table payout (
    id uuid primary key,
    provider_account_id text not null,
    kind text not null default 'PAYOUT' check (kind = 'PAYOUT'),
    reference text not null,
    currency text not null check (currency ~ '^[A-Z]{3}$'),
    amount bigint not null check (amount > 0),
    beneficiary jsonb not null,
    remittance_info text,
    failure_code text check (failure_code ~ '^[A-Z]{2}[0-9]{2}$'),
    due_at timestamptz not null,
    created_at timestamptz not null default now(),
    finalized_at timestamptz,
    unique (provider_account_id, reference),
    foreign key (provider_account_id, kind, reference) references operation_reference
);

create index payout_due on payout (due_at) where finalized_at is null;

create table funding (
    provider_account_id text not null references provider_account,
    reference text not null check (length(reference) between 1 and 128),
    request_hash bytea not null check (length(request_hash) = 32),
    currency text not null,
    sequence bigint not null,
    created_at timestamptz not null default now(),
    primary key (provider_account_id, reference),
    foreign key (provider_account_id, currency, sequence) references cash_movement
);
