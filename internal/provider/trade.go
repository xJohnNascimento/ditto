package provider

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type TradeRequest struct {
	Reference    string    `json:"-"`
	QuoteID      uuid.UUID `json:"quote_id"`
	SellCurrency string    `json:"sell_currency"`
	BuyCurrency  string    `json:"buy_currency"`
	SellAmount   int64     `json:"sell_amount"`
}

type TradeStatus string

const (
	TradeStatusFilled   TradeStatus = "FILLED"
	TradeStatusRejected TradeStatus = "REJECTED"
	TradeStatusClosed   TradeStatus = "CLOSED"
)

type Trade struct {
	Reference    string
	Status       TradeStatus
	DropReply    bool
	Faults       []string
	HasTerms     bool
	ID           uuid.UUID
	Reason       string
	SellCurrency string
	BuyCurrency  string
	SellAmount   int64
	BuyAmount    int64
	RateE8       int64
	ExecutedAt   time.Time
}

func (s *Service) CreateTrade(ctx context.Context, account string, req TradeRequest) (Trade, bool, error) {
	hash := requestHash(req)
	var (
		result  Trade
		created bool
		delayed bool
	)
	var quoteReference string
	err := s.pool.QueryRow(ctx, "select client_reference from quote where id = $1 and provider_account_id = $2", req.QuoteID, account).Scan(&quoteReference)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return Trade{}, false, fmt.Errorf("create trade: %w", err)
	}
	if err == nil {
		if delayed, err = s.delayBeforeExecution(ctx, targetQuote, quoteReference); err != nil {
			return Trade{}, false, fmt.Errorf("create trade: %w", err)
		}
	}
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		c, err := claimReference(ctx, tx, account, kindTrade, req.Reference, hash)
		if err != nil {
			return err
		}
		if !c.created {
			if created, err = replayOrConflict(ctx, tx, account, kindTrade, req.Reference, c, hash); err != nil {
				return err
			}
			result, err = tradeByReference(ctx, tx, account, req.Reference)
			return err
		}
		result, err = s.executeTrade(ctx, tx, account, req)
		created = err == nil
		return err
	})
	if err != nil {
		return Trade{}, false, fmt.Errorf("create trade: %w", err)
	}
	if delayed {
		result.Faults = append(result.Faults, DelayBeforeExecution)
	}
	return result, created, nil
}

func (s *Service) executeTrade(ctx context.Context, tx pgx.Tx, account string, req TradeRequest) (Trade, error) {
	var (
		q       Quote
		expired bool
	)
	// The reference claim may have waited on a lock, so expiry is judged by the clock at this
	// decision, not by now(), which is fixed when the transaction began.
	err := tx.QueryRow(ctx, `
		select client_reference, sell_currency, buy_currency, sell_amount, rate_e8, buy_amount, clock_timestamp() > expires_at
		from quote where id = $1 and provider_account_id = $2`,
		req.QuoteID, account,
	).Scan(&q.ClientReference, &q.SellCurrency, &q.BuyCurrency, &q.SellAmount, &q.RateE8, &q.BuyAmount, &expired)
	if errors.Is(err, pgx.ErrNoRows) {
		return Trade{}, invalid("quote %s is unknown", req.QuoteID)
	}
	if err != nil {
		return Trade{}, err
	}
	if q.SellCurrency != req.SellCurrency || q.BuyCurrency != req.BuyCurrency || q.SellAmount != req.SellAmount {
		return Trade{}, invalid("the trade does not match quote %s", req.QuoteID)
	}
	id, err := uuid.NewV7()
	if err != nil {
		return Trade{}, err
	}

	t, err := settleTrade(ctx, tx, account, q, expired, Trade{
		Reference: req.Reference, Status: TradeStatusFilled, HasTerms: true, ID: id,
		SellCurrency: q.SellCurrency, BuyCurrency: q.BuyCurrency, SellAmount: q.SellAmount, BuyAmount: q.BuyAmount, RateE8: q.RateE8,
	})
	if err != nil {
		return Trade{}, err
	}
	err = tx.QueryRow(ctx, `
		insert into trade (id, provider_account_id, reference, quote_id, sell_currency, buy_currency, sell_amount, buy_amount, rate_e8, reason)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, nullif($10, ''))
		returning executed_at`,
		id, account, req.Reference, req.QuoteID, t.SellCurrency, t.BuyCurrency, t.SellAmount, t.BuyAmount, t.RateE8, t.Reason,
	).Scan(&t.ExecutedAt)
	if err != nil {
		return Trade{}, err
	}
	drop, err := consume(ctx, tx, targetQuote, q.ClientReference, TradeCommitDropReply)
	if err != nil {
		return Trade{}, err
	}
	if drop != nil {
		t.DropReply, t.Faults = true, append(t.Faults, drop.Fault)
	}
	return t, setState(ctx, tx, account, kindTrade, req.Reference, t.Status)
}

// settleTrade decides the trade at its serialized execution point. A fault is consumed only when
// it can still change the outcome, so an expired quote leaves its armed fault for a later trade,
// and a rejected trade keeps the quoted amount rather than a fault-altered one.
func settleTrade(ctx context.Context, tx pgx.Tx, account string, q Quote, expired bool, t Trade) (Trade, error) {
	if expired {
		t.Status, t.Reason = TradeStatusRejected, "QUOTE_EXPIRED"
		return t, nil
	}
	arm, err := consume(ctx, tx, targetQuote, q.ClientReference, TradeReject, FXFillDifference)
	if err != nil {
		return Trade{}, err
	}
	delivered := q.BuyAmount
	if arm != nil {
		t.Faults = append(t.Faults, arm.Fault)
		if arm.Fault == TradeReject {
			t.Status, t.Reason = TradeStatusRejected, "LAST_LOOK"
			return t, nil
		}
		delivered = max(1, delivered+arm.Args.Delta)
	}
	_, sufficient, err := move(ctx, tx, account, t.Reference, t.ID, 1, []leg{
		{currency: t.SellCurrency, amount: -t.SellAmount, kind: "TRADE_SELL"},
		{currency: t.BuyCurrency, amount: delivered, kind: "TRADE_BUY"},
	})
	if err != nil {
		return Trade{}, err
	}
	if !sufficient {
		t.Status, t.Reason = TradeStatusRejected, "INSUFFICIENT_BALANCE"
		return t, nil
	}
	t.BuyAmount = delivered
	return t, nil
}

func (s *Service) Trade(ctx context.Context, account, reference string) (Trade, error) {
	t, err := tradeByReference(ctx, s.pool, account, reference)
	if errors.Is(err, pgx.ErrNoRows) {
		return Trade{}, ErrNotFound
	}
	if err != nil {
		return Trade{}, fmt.Errorf("get trade: %w", err)
	}
	return t, nil
}

func tradeByReference(ctx context.Context, q querier, account, reference string) (Trade, error) {
	var (
		t                           Trade
		id                          *uuid.UUID
		sell, buy, reason           *string
		sellAmount, buyAmount, rate *int64
		executedAt                  *time.Time
	)
	err := q.QueryRow(ctx, `
		select r.reference, r.state, t.id, t.sell_currency, t.buy_currency, t.sell_amount, t.buy_amount, t.rate_e8, t.reason, t.executed_at
		from operation_reference r
		left join trade t on t.provider_account_id = r.provider_account_id and t.reference = r.reference
		where r.provider_account_id = $1 and r.kind = 'TRADE' and r.reference = $2`,
		account, reference,
	).Scan(&t.Reference, &t.Status, &id, &sell, &buy, &sellAmount, &buyAmount, &rate, &reason, &executedAt)
	if err != nil || id == nil {
		return t, err
	}
	t.HasTerms, t.ID = true, *id
	t.SellCurrency, t.BuyCurrency, t.SellAmount, t.BuyAmount, t.RateE8, t.ExecutedAt = *sell, *buy, *sellAmount, *buyAmount, *rate, *executedAt
	if reason != nil {
		t.Reason = *reason
	}
	return t, nil
}
