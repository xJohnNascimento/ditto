package provider

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
)

type Admission struct {
	Admitted   bool
	RetryAfter time.Duration
	Fault      string
}

// Admit takes one token from the account's bucket in a single locked statement, so every Ditto
// instance shares the same budget. A refusal takes nothing and says when a token will be there.
func (s *Service) Admit(ctx context.Context, account string) (Admission, error) {
	var (
		admission    Admission
		available    float64
		rate         float64
		throttledFor float64
	)
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		arm, err := consume(ctx, tx, targetAccount, account, RateLimit)
		if err != nil {
			return err
		}
		if arm != nil {
			admission.Fault = arm.Fault
			_, err := tx.Exec(ctx, "update request_budget set throttled_until = clock_timestamp() + make_interval(secs => $2) where provider_account_id = $1",
				account, (time.Duration(arm.Args.DurationMS) * time.Millisecond).Seconds())
			if err != nil {
				return err
			}
		}
		// clock_timestamp, not now(): now() is frozen at the transaction's start, but the refill must
		// count the real time since the last admission, including any wait for the row lock. The CTE
		// locks the bucket and refills it up to burst; the update then spends a token only when one is
		// there and no RATE_LIMIT window is open. A refusal still stores the refill, so elapsed time is
		// never counted twice.
		return tx.QueryRow(ctx, `
		with clock as (select clock_timestamp() as now),
		current as (
			select b.provider_account_id,
			       least(b.burst::numeric, b.tokens + extract(epoch from clock.now - b.updated_at)::numeric * b.rate_per_second) as available,
			       b.rate_per_second,
			       coalesce(extract(epoch from b.throttled_until - clock.now), 0) as throttled_for,
			       clock.now
			from request_budget b, clock
			where b.provider_account_id = $1
			for update of b
		)
		update request_budget b
		set tokens = case when c.available >= 1 and c.throttled_for <= 0 then c.available - 1 else c.available end,
		    updated_at = c.now
		from current c
		where b.provider_account_id = c.provider_account_id
		returning c.available >= 1 and c.throttled_for <= 0, c.available::float8, c.rate_per_second::float8, c.throttled_for::float8`,
			account,
		).Scan(&admission.Admitted, &available, &rate, &throttledFor)
	})
	if err != nil {
		return Admission{}, fmt.Errorf("admit request: %w", err)
	}
	if admission.Admitted {
		return admission, nil
	}
	wait := math.Max((1-available)/rate, throttledFor)
	admission.RetryAfter = time.Duration(math.Ceil(math.Max(wait, 1))) * time.Second
	return admission, nil
}

type Observation struct {
	RequestID string
	Account   string
	Route     string
	Reference string
	Status    int
	Throttled bool
	Fault     string
	Received  time.Time
}

func (s *Service) Observe(ctx context.Context, o Observation) error {
	_, err := s.pool.Exec(ctx, `
		insert into request_observation (request_id, provider_account_id, route, reference, status, throttled, fault, received_at)
		values ($1, $2, $3, nullif($4, ''), $5, $6, nullif($7, ''), $8)`,
		o.RequestID, o.Account, o.Route, o.Reference, o.Status, o.Throttled, o.Fault, o.Received,
	)
	if err != nil {
		return fmt.Errorf("record observation: %w", err)
	}
	return nil
}
