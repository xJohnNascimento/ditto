package provider

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const rateScale = 100_000_000

type QuoteRequest struct {
	ClientReference string `json:"client_reference"`
	SellCurrency    string `json:"sell_currency"`
	BuyCurrency     string `json:"buy_currency"`
	SellAmount      int64  `json:"sell_amount"`
}

type Quote struct {
	QuoteRequest
	ID          uuid.UUID
	RateE8      int64
	BuyAmount   int64
	ExpiresAt   time.Time
	requestHash []byte
}

func (s *Service) CreateQuote(ctx context.Context, account string, req QuoteRequest) (Quote, bool, error) {
	q, created, err := s.createQuote(ctx, account, req)
	if err != nil {
		return Quote{}, false, fmt.Errorf("create quote: %w", err)
	}
	return q, created, nil
}

func (s *Service) createQuote(ctx context.Context, account string, req QuoteRequest) (Quote, bool, error) {
	if req.SellCurrency == req.BuyCurrency {
		return Quote{}, false, invalid("sell and buy currencies must differ")
	}
	hash := requestHash(req)
	existing, err := s.quoteByReference(ctx, account, req.ClientReference)
	if err == nil {
		return replayedQuote(existing, hash)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Quote{}, false, err
	}

	var rateE8 int64
	err = s.pool.QueryRow(ctx, "select rate_e8 from rate where sell_currency = $1 and buy_currency = $2",
		req.SellCurrency, req.BuyCurrency).Scan(&rateE8)
	if errors.Is(err, pgx.ErrNoRows) {
		return Quote{}, false, invalid("the pair %s/%s is not offered", req.SellCurrency, req.BuyCurrency)
	}
	if err != nil {
		return Quote{}, false, err
	}
	buy, err := convert(req.SellAmount, rateE8)
	if err != nil {
		return Quote{}, false, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return Quote{}, false, err
	}

	q := Quote{QuoteRequest: req, ID: id, RateE8: rateE8, BuyAmount: buy}
	err = s.pool.QueryRow(ctx, `
		insert into quote (id, provider_account_id, client_reference, request_hash, sell_currency, buy_currency,
		                   sell_amount, rate_e8, buy_amount, expires_at)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, now() + make_interval(secs => $10))
		on conflict (provider_account_id, client_reference) do nothing
		returning expires_at`,
		id, account, req.ClientReference, hash, req.SellCurrency, req.BuyCurrency, req.SellAmount, rateE8, buy,
		s.opts.QuoteTTL.Seconds(),
	).Scan(&q.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		existing, err := s.quoteByReference(ctx, account, req.ClientReference)
		if err != nil {
			return Quote{}, false, err
		}
		return replayedQuote(existing, hash)
	}
	return q, err == nil, err
}

func replayedQuote(existing Quote, hash []byte) (Quote, bool, error) {
	if !bytes.Equal(existing.requestHash, hash) {
		return Quote{}, false, ErrConflict
	}
	return existing, false, nil
}

const quoteColumns = `id, client_reference, sell_currency, buy_currency, sell_amount, rate_e8, buy_amount, expires_at, request_hash`

func (s *Service) quoteByReference(ctx context.Context, account, reference string) (Quote, error) {
	return scanQuote(s.pool.QueryRow(ctx, "select "+quoteColumns+" from quote where provider_account_id = $1 and client_reference = $2", account, reference))
}

func scanQuote(row pgx.Row) (Quote, error) {
	var q Quote
	err := row.Scan(&q.ID, &q.ClientReference, &q.SellCurrency, &q.BuyCurrency, &q.SellAmount, &q.RateE8, &q.BuyAmount, &q.ExpiresAt, &q.requestHash)
	return q, err
}

func convert(amount, rateE8 int64) (int64, error) {
	product := new(big.Int).Mul(big.NewInt(amount), big.NewInt(rateE8))
	product.Quo(product, big.NewInt(rateScale))
	if !product.IsInt64() {
		return 0, invalid("the converted amount is too large")
	}
	if product.Sign() <= 0 {
		return 0, invalid("the sell amount converts to nothing")
	}
	return product.Int64(), nil
}
