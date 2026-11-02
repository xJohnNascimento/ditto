package provider

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

type RouteCount struct {
	Route     string
	Count     int64
	Throttled int64
}

type RequestStats struct {
	Since         time.Time
	Routes        []RouteCount
	PeakPerSecond int
}

func (s *Service) RequestStats(ctx context.Context, since time.Time) (RequestStats, error) {
	stats := RequestStats{Since: since}
	rows, err := s.pool.Query(ctx, `
		select route, count(*), count(*) filter (where throttled)
		from request_observation where received_at >= $1
		group by route order by route`,
		since,
	)
	if err != nil {
		return stats, fmt.Errorf("request stats: %w", err)
	}
	stats.Routes, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (RouteCount, error) {
		var c RouteCount
		return c, row.Scan(&c.Route, &c.Count, &c.Throttled)
	})
	if err != nil {
		return stats, fmt.Errorf("request stats: %w", err)
	}
	err = s.pool.QueryRow(ctx, `
		select coalesce(max(n), 0) from (
			select count(*) as n from request_observation
			where received_at >= $1 and not throttled
			group by provider_account_id, date_trunc('second', received_at)
		) per_second`,
		since,
	).Scan(&stats.PeakPerSecond)
	if err != nil {
		return stats, fmt.Errorf("request stats: %w", err)
	}
	return stats, nil
}

// Reset clears every fact of the dataset and restores each account's budget and the configured
// rates. The API allows it only in the local and CI profiles, for a matching dataset id.
func (s *Service) Reset(ctx context.Context) error {
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			truncate statement_line, statement, webhook_delivery, webhook_event, scenario_consumption, scenario_arm,
			         request_observation, funding, trade, payout, operation_reference, cash_movement, cash_account, quote, rate`)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, "update request_budget set tokens = burst, updated_at = clock_timestamp(), throttled_until = null")
		if err != nil {
			return err
		}
		for _, r := range s.rates {
			if _, err := tx.Exec(ctx, "insert into rate (sell_currency, buy_currency, rate_e8) values ($1, $2, $3)", r.Sell, r.Buy, r.RateE8); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("reset: %w", err)
	}
	return nil
}
