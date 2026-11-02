-- +goose Up

create table statement (
    id uuid primary key,
    provider_account_id text not null references provider_account,
    previous_id uuid references statement,
    captured_at timestamptz not null,
    content_hash text not null,
    line_count bigint not null check (line_count >= 0),
    currencies jsonb not null,
    unique (id, provider_account_id)
);

-- Lines are frozen as the exact JSON a page returns, so a page never reads the live tables and the
-- content hash covers the bytes a client receives.
create table statement_line (
    statement_id uuid not null references statement,
    line_no integer not null check (line_no > 0),
    kind text not null check (kind in ('MOVEMENT', 'OUTCOME', 'BALANCE')),
    operation text check (operation in ('TRADE', 'PAYOUT')),
    reference text,
    body text not null,
    primary key (statement_id, line_no),
    check ((kind = 'OUTCOME') = (operation is not null))
);

create index statement_line_outcome on statement_line (operation, reference) where kind = 'OUTCOME';

alter table payout add column lagged_until timestamptz;
