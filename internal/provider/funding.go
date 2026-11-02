package provider

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type FundingRequest struct {
	Account   string `json:"account"`
	Reference string `json:"-"`
	Currency  string `json:"currency"`
	Amount    int64  `json:"amount"`
}

// Fund books a signed funding movement once per reference. Funding has no fence, so concurrent
// calls serialize on an advisory lock of the reference instead of a reference row.
func (s *Service) Fund(ctx context.Context, req FundingRequest) (Movement, bool, error) {
	if req.Amount == 0 {
		return Movement{}, false, invalid("a funding amount cannot be zero")
	}
	hash := requestHash(req)
	var (
		result  Movement
		created bool
	)
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "select pg_advisory_xact_lock(hashtextextended($1 || '/funding/' || $2, 0))", req.Account, req.Reference); err != nil {
			return err
		}
		var (
			existingHash []byte
			m            Movement
		)
		err := tx.QueryRow(ctx, `
			select f.request_hash, m.sequence, m.currency, m.amount, m.balance_after, m.kind, m.reference
			from funding f
			join cash_movement m on m.provider_account_id = f.provider_account_id and m.currency = f.currency and m.sequence = f.sequence
			where f.provider_account_id = $1 and f.reference = $2`,
			req.Account, req.Reference,
		).Scan(&existingHash, &m.Sequence, &m.Currency, &m.Amount, &m.BalanceAfter, &m.Kind, &m.Reference)
		if err == nil {
			if !bytes.Equal(existingHash, hash) {
				return ErrConflict
			}
			result = m
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}

		var known bool
		if err := tx.QueryRow(ctx, "select exists (select from provider_account where id = $1)", req.Account).Scan(&known); err != nil {
			return err
		}
		if !known {
			return invalid("account %q is unknown", req.Account)
		}
		effect, err := uuid.NewV7()
		if err != nil {
			return err
		}
		movements, sufficient, err := move(ctx, tx, req.Account, req.Reference, effect, 1, []leg{{currency: req.Currency, amount: req.Amount, kind: "FUNDING"}})
		if err != nil {
			return err
		}
		if !sufficient {
			return invalid("the withdrawal exceeds the %s balance", req.Currency)
		}
		result, created = movements[0], true
		_, err = tx.Exec(ctx, "insert into funding (provider_account_id, reference, request_hash, currency, sequence) values ($1, $2, $3, $4, $5)",
			req.Account, req.Reference, hash, req.Currency, result.Sequence)
		return err
	})
	if err != nil {
		return Movement{}, false, fmt.Errorf("fund account: %w", err)
	}
	return result, created, nil
}
