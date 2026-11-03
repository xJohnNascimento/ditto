-- +goose Up

-- A firm quote buys one fill. A trade without a rejection reason is a fill, so rejected attempts
-- (expired, last look, insufficient balance) leave the quote usable.
create unique index trade_one_fill_per_quote on trade (provider_account_id, quote_id) where reason is null;
