package provider

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// CloseTrade answers a trade's terminal outcome or closes its reference for good. Trades execute
// inside their create transaction, so a committed trade reference is always terminal.
func (s *Service) CloseTrade(ctx context.Context, account, reference string) (TradeStatus, error) {
	var state TradeStatus
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		state, err = fence(ctx, tx, account, kindTrade, reference, TradeStatusClosed)
		return err
	})
	if err != nil {
		return "", fmt.Errorf("close trade: %w", err)
	}
	return state, nil
}

// CancelPayout cancels and refunds a payout that has not executed, fences an unknown reference for
// good, and leaves anything else unchanged. It takes the reference lock, so it serializes with the
// create and with every scheduled step.
func (s *Service) CancelPayout(ctx context.Context, account, reference string) (PayoutStatus, error) {
	var state PayoutStatus
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		state, err = fence(ctx, tx, account, kindPayout, reference, PayoutStatusCancelled)
		if err != nil {
			return err
		}
		if state != PayoutStatusAccepted && state != PayoutStatusProcessing {
			return nil
		}
		p, err := payoutByReference(ctx, tx, account, reference)
		if err != nil {
			return err
		}
		if err := refund(ctx, tx, account, p); err != nil {
			return err
		}
		state = PayoutStatusCancelled
		return s.finishPayout(ctx, tx, account, p, state, "")
	})
	if err != nil {
		return "", fmt.Errorf("cancel payout: %w", err)
	}
	return state, nil
}

func fence[S ~string](ctx context.Context, tx pgx.Tx, account, kind, reference string, tombstone S) (S, error) {
	tag, err := tx.Exec(ctx, `
		insert into operation_reference (provider_account_id, kind, reference, state)
		values ($1, $2, $3, $4)
		on conflict do nothing`,
		account, kind, reference, tombstone,
	)
	if err != nil || tag.RowsAffected() == 1 {
		return tombstone, err
	}
	c, err := lockReference(ctx, tx, account, kind, reference)
	return S(c.state), err
}

func refund(ctx context.Context, tx pgx.Tx, account string, p Payout) error {
	_, _, err := move(ctx, tx, account, p.Reference, p.ID, 2, []leg{{currency: p.Currency, amount: p.Amount, kind: "PAYOUT_REFUND"}})
	return err
}

func (s *Service) finishPayout(ctx context.Context, tx pgx.Tx, account string, p Payout, status PayoutStatus, failureCode string) error {
	_, err := tx.Exec(ctx, `
		update payout set finalized_at = now(), failure_code = nullif($3, '')
		where provider_account_id = $1 and reference = $2`,
		account, p.Reference, failureCode,
	)
	if err != nil {
		return err
	}
	if err := setState(ctx, tx, account, kindPayout, p.Reference, status); err != nil {
		return err
	}
	p.Status, p.Sequence, p.FailureCode = status, sequenceOf[status], failureCode
	return s.emitPayoutEvent(ctx, tx, account, p)
}
