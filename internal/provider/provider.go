package provider

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/xJohnNascimento/ditto/internal/config"
)

var (
	ErrConflict  = errors.New("the reference already holds a different request")
	ErrNotFound  = errors.New("not visible now")
	ErrQuoteUsed = errors.New("the quote already filled a trade")
)

type ValidationError struct {
	Message string
}

func (e *ValidationError) Error() string {
	return e.Message
}

func invalid(format string, args ...any) error {
	return &ValidationError{Message: fmt.Sprintf(format, args...)}
}

type Options struct {
	QuoteTTL       time.Duration
	RatePerSecond  float64
	Burst          int
	PayoutStep     time.Duration
	WebhookBackoff time.Duration
	WebhookWindow  time.Duration
}

type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

type Service struct {
	pool      *pgxpool.Pool
	opts      Options
	endpoints map[string]Endpoint
	rates     []config.Rate
}

func New(pool *pgxpool.Pool, opts Options) *Service {
	return &Service{pool: pool, opts: opts, endpoints: make(map[string]Endpoint)}
}

// Bootstrap registers the configured accounts and their request budgets, and seeds every rate that
// is not set yet, so a rate changed through the admin API survives a restart.
func (s *Service) Bootstrap(ctx context.Context, accounts []config.Account, rates []config.Rate) error {
	s.rates = rates
	for _, a := range accounts {
		if a.WebhookURL == "" {
			continue
		}
		secrets, err := ParseWebhookSecrets(a.WebhookSecret)
		if err != nil {
			return fmt.Errorf("account %q: %w", a.ID, err)
		}
		s.endpoints[a.ID] = Endpoint{URL: a.WebhookURL, Secrets: secrets}
	}
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		for _, a := range accounts {
			if _, err := tx.Exec(ctx, "insert into provider_account (id) values ($1) on conflict do nothing", a.ID); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `
				insert into request_budget (provider_account_id, rate_per_second, burst, tokens, updated_at)
				values ($1, $2, $3, $4, clock_timestamp())
				on conflict (provider_account_id) do update
				set rate_per_second = excluded.rate_per_second, burst = excluded.burst`,
				a.ID, s.opts.RatePerSecond, s.opts.Burst, float64(s.opts.Burst),
			)
			if err != nil {
				return err
			}
		}
		for _, r := range rates {
			_, err := tx.Exec(ctx,
				"insert into rate (sell_currency, buy_currency, rate_e8) values ($1, $2, $3) on conflict do nothing",
				r.Sell, r.Buy, r.RateE8,
			)
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	}
	return nil
}

func (s *Service) SetRate(ctx context.Context, sell, buy string, rateE8 int64) error {
	if sell == buy {
		return invalid("a rate needs two different currencies")
	}
	_, err := s.pool.Exec(ctx, `
		insert into rate (sell_currency, buy_currency, rate_e8) values ($1, $2, $3)
		on conflict (sell_currency, buy_currency) do update set rate_e8 = excluded.rate_e8, updated_at = now()`,
		sell, buy, rateE8,
	)
	if err != nil {
		return fmt.Errorf("set rate %s/%s: %w", sell, buy, err)
	}
	return nil
}

// requestHash covers every field except the reference, which keys the row, in a fixed struct
// order, so field order in the JSON a client sent never changes it.
func requestHash(request any) []byte {
	encoded, err := json.Marshal(request)
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(encoded)
	return sum[:]
}
