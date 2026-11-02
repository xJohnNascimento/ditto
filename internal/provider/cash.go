package provider

import (
	"context"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type Movement struct {
	Sequence     int64
	Currency     string
	Amount       int64
	BalanceAfter int64
	Kind         string
	Reference    string
}

type leg struct {
	currency string
	amount   int64
	kind     string
}

// move locks the account's cash rows in currency order, refuses the whole effect if any balance
// would go negative, and otherwise appends one movement per leg with the next sequence of its
// currency. Allocating sequences under the lock keeps every currency's history gap-free, and the
// unique (effect, leg) pair makes a second debit or refund of the same effect impossible.
func move(ctx context.Context, tx pgx.Tx, account, reference string, effect uuid.UUID, firstLeg int, legs []leg) ([]Movement, bool, error) {
	currencies := make([]string, 0, len(legs))
	for _, l := range legs {
		currencies = append(currencies, l.currency)
	}
	slices.Sort(currencies)
	currencies = slices.Compact(currencies)
	for _, currency := range currencies {
		if _, err := tx.Exec(ctx, "insert into cash_account (provider_account_id, currency) values ($1, $2) on conflict do nothing", account, currency); err != nil {
			return nil, false, err
		}
	}

	balances := make(map[string]int64, len(currencies))
	sequences := make(map[string]int64, len(currencies))
	rows, err := tx.Query(ctx, `
		select currency, balance, last_sequence from cash_account
		where provider_account_id = $1 and currency = any($2)
		order by currency
		for update`,
		account, currencies,
	)
	if err != nil {
		return nil, false, err
	}
	for rows.Next() {
		var (
			currency          string
			balance, sequence int64
		)
		if err := rows.Scan(&currency, &balance, &sequence); err != nil {
			rows.Close()
			return nil, false, err
		}
		balances[currency], sequences[currency] = balance, sequence
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, false, err
	}

	for _, l := range legs {
		balances[l.currency] += l.amount
		if balances[l.currency] < 0 {
			return nil, false, nil
		}
	}
	movements := make([]Movement, len(legs))
	for i, l := range legs {
		sequences[l.currency]++
		var after int64
		err := tx.QueryRow(ctx, `
			update cash_account set balance = balance + $3, last_sequence = $4
			where provider_account_id = $1 and currency = $2
			returning balance`,
			account, l.currency, l.amount, sequences[l.currency],
		).Scan(&after)
		if err != nil {
			return nil, false, err
		}
		_, err = tx.Exec(ctx, `
			insert into cash_movement (provider_account_id, currency, sequence, amount, balance_after, kind, reference, effect_id, leg)
			values ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
			account, l.currency, sequences[l.currency], l.amount, after, l.kind, reference, effect, firstLeg+i,
		)
		if err != nil {
			return nil, false, err
		}
		movements[i] = Movement{Sequence: sequences[l.currency], Currency: l.currency, Amount: l.amount, BalanceAfter: after, Kind: l.kind, Reference: reference}
	}
	return movements, true, nil
}
