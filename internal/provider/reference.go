package provider

import (
	"bytes"
	"context"

	"github.com/jackc/pgx/v5"
)

const (
	kindTrade  = "TRADE"
	kindPayout = "PAYOUT"
	pending    = "PENDING"
)

type claim struct {
	state       string
	requestHash []byte
	created     bool
}

// claimReference inserts the reference as PENDING, holding its row lock until commit, or locks the
// existing row. The read after a conflict is a new statement, so it sees the winner's commit.
func claimReference(ctx context.Context, tx pgx.Tx, account, kind, reference string, hash []byte) (claim, error) {
	tag, err := tx.Exec(ctx, `
		insert into operation_reference (provider_account_id, kind, reference, request_hash, state)
		values ($1, $2, $3, $4, 'PENDING')
		on conflict do nothing`,
		account, kind, reference, hash,
	)
	if err != nil {
		return claim{}, err
	}
	if tag.RowsAffected() == 1 {
		return claim{state: pending, requestHash: hash, created: true}, nil
	}
	return lockReference(ctx, tx, account, kind, reference)
}

func lockReference(ctx context.Context, tx pgx.Tx, account, kind, reference string) (claim, error) {
	var c claim
	err := tx.QueryRow(ctx, `
		select state, request_hash from operation_reference
		where provider_account_id = $1 and kind = $2 and reference = $3
		for update`,
		account, kind, reference,
	).Scan(&c.state, &c.requestHash)
	return c, err
}

// recordFirstBody lets the first create after a fence keep its body, so a replay of that body is
// recognised and any other body is a conflict; the fence itself still decides the outcome.
func recordFirstBody(ctx context.Context, tx pgx.Tx, account, kind, reference string, hash []byte) error {
	_, err := tx.Exec(ctx, `
		update operation_reference set request_hash = $4, updated_at = now()
		where provider_account_id = $1 and kind = $2 and reference = $3`,
		account, kind, reference, hash,
	)
	return err
}

// replayOrConflict resolves a create that found its reference already claimed: a fence without a
// body takes this body and reports a first answer, the same body replays, another body conflicts.
func replayOrConflict(ctx context.Context, tx pgx.Tx, account, kind, reference string, c claim, hash []byte) (bool, error) {
	if c.requestHash == nil {
		return true, recordFirstBody(ctx, tx, account, kind, reference, hash)
	}
	if !bytes.Equal(c.requestHash, hash) {
		return false, ErrConflict
	}
	return false, nil
}

func setState[S ~string](ctx context.Context, tx pgx.Tx, account, kind, reference string, state S) error {
	_, err := tx.Exec(ctx, `
		update operation_reference set state = $4, version = version + 1, updated_at = now()
		where provider_account_id = $1 and kind = $2 and reference = $3`,
		account, kind, reference, state,
	)
	return err
}
