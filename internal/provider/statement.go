package provider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const PageSize = 100

type StatementCurrency struct {
	Currency       string `json:"currency"`
	OpeningBalance string `json:"opening_balance"`
	ClosingBalance string `json:"closing_balance"`
	FromSequence   int64  `json:"from_sequence"`
	ToSequence     int64  `json:"to_sequence"`
}

type Statement struct {
	ID          uuid.UUID
	CapturedAt  time.Time
	ContentHash string
	LineCount   int64
	Currencies  []StatementCurrency
}

type lineMovement struct {
	Sequence     int64  `json:"sequence"`
	Currency     string `json:"currency"`
	Amount       string `json:"amount"`
	BalanceAfter string `json:"balance_after"`
	Kind         string `json:"kind"`
	Reference    string `json:"reference"`
}

type lineOutcome struct {
	Operation   string    `json:"operation"`
	Reference   string    `json:"reference"`
	Status      string    `json:"status"`
	Currency    string    `json:"currency,omitempty"`
	Amount      string    `json:"amount,omitempty"`
	FailureCode string    `json:"failure_code,omitempty"`
	FinalizedAt time.Time `json:"finalized_at"`
}

type line struct {
	LineID   string             `json:"line_id"`
	Kind     string             `json:"kind"`
	Movement *lineMovement      `json:"movement,omitempty"`
	Outcome  *lineOutcome       `json:"outcome,omitempty"`
	Balance  *StatementCurrency `json:"balance,omitempty"`
}

// CaptureStatement freezes one repeatable-read view of an account. A statement after another
// opens at its closing balances and sequence watermarks and reports only outcomes the chain has
// not reported yet, so consecutive statements roll forward exactly with no gaps or repeats.
func (s *Service) CaptureStatement(ctx context.Context, account string, after *uuid.UUID) (Statement, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return Statement{}, fmt.Errorf("capture statement: %w", err)
	}
	st := Statement{ID: id}
	err = pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead}, func(tx pgx.Tx) error {
		// The first query of a repeatable-read transaction takes its snapshot, so reading the clock
		// first pins the view every later read shares and ties captured_at to it.
		if err := tx.QueryRow(ctx, "select now()").Scan(&st.CapturedAt); err != nil {
			return err
		}
		previous, err := previousBalances(ctx, tx, account, after)
		if err != nil {
			return err
		}
		currencies, lines, err := statementLines(ctx, tx, account, after, previous)
		if err != nil {
			return err
		}
		st.Currencies = currencies
		hash := sha256.New()
		bodies := make([][]byte, len(lines))
		for i, l := range lines {
			l.LineID = strconv.Itoa(i + 1)
			if bodies[i], err = json.Marshal(l); err != nil {
				return err
			}
			hash.Write(bodies[i])
		}
		st.LineCount, st.ContentHash = int64(len(lines)), hex.EncodeToString(hash.Sum(nil))
		_, err = tx.Exec(ctx, `
			insert into statement (id, provider_account_id, previous_id, captured_at, content_hash, line_count, currencies)
			values ($1, $2, $3, $4, $5, $6, $7)`,
			st.ID, account, after, st.CapturedAt, st.ContentHash, st.LineCount, st.Currencies,
		)
		if err != nil {
			return err
		}
		for i, l := range lines {
			var operation, reference *string
			if l.Outcome != nil {
				operation, reference = &l.Outcome.Operation, &l.Outcome.Reference
			}
			_, err := tx.Exec(ctx, `
				insert into statement_line (statement_id, line_no, kind, operation, reference, body)
				values ($1, $2, $3, $4, $5, $6)`,
				st.ID, i+1, l.Kind, operation, reference, string(bodies[i]),
			)
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return Statement{}, fmt.Errorf("capture statement: %w", err)
	}
	return st, nil
}

func previousBalances(ctx context.Context, tx pgx.Tx, account string, after *uuid.UUID) (map[string]StatementCurrency, error) {
	previous := map[string]StatementCurrency{}
	if after == nil {
		return previous, nil
	}
	var manifest []byte
	err := tx.QueryRow(ctx, "select currencies from statement where id = $1 and provider_account_id = $2", *after, account).Scan(&manifest)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var currencies []StatementCurrency
	if err := json.Unmarshal(manifest, &currencies); err != nil {
		return nil, err
	}
	for _, c := range currencies {
		previous[c.Currency] = c
	}
	return previous, nil
}

// statementLines returns the manifest's currencies and the statement's lines: a balance line per
// currency, then the movements of each, then the outcomes.
func statementLines(ctx context.Context, tx pgx.Tx, account string, after *uuid.UUID, previous map[string]StatementCurrency) ([]StatementCurrency, []line, error) {
	balances, err := statementBalances(ctx, tx, account, previous)
	if err != nil {
		return nil, nil, err
	}
	movements, err := statementMovements(ctx, tx, account, balances)
	if err != nil {
		return nil, nil, err
	}
	outcomes, err := statementOutcomes(ctx, tx, account, after)
	if err != nil {
		return nil, nil, err
	}
	lines := make([]line, 0, len(balances)+len(movements)+len(outcomes))
	for i := range balances {
		lines = append(lines, line{Kind: "BALANCE", Balance: &balances[i]})
	}
	lines = append(lines, movements...)
	return balances, append(lines, outcomes...), nil
}

func statementBalances(ctx context.Context, tx pgx.Tx, account string, previous map[string]StatementCurrency) ([]StatementCurrency, error) {
	rows, err := tx.Query(ctx, "select currency, balance, last_sequence from cash_account where provider_account_id = $1 order by currency", account)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (StatementCurrency, error) {
		var (
			c                 StatementCurrency
			balance, sequence int64
		)
		if err := row.Scan(&c.Currency, &balance, &sequence); err != nil {
			return c, err
		}
		c.OpeningBalance, c.FromSequence = "0", 1
		if prev, ok := previous[c.Currency]; ok {
			c.OpeningBalance, c.FromSequence = prev.ClosingBalance, prev.ToSequence+1
		}
		c.ClosingBalance, c.ToSequence = strconv.FormatInt(balance, 10), sequence
		return c, nil
	})
}

func statementMovements(ctx context.Context, tx pgx.Tx, account string, balances []StatementCurrency) ([]line, error) {
	var movements []line
	for _, c := range balances {
		rows, err := tx.Query(ctx, `
			select sequence, amount, balance_after, kind, reference from cash_movement
			where provider_account_id = $1 and currency = $2 and sequence between $3 and $4
			order by sequence`,
			account, c.Currency, c.FromSequence, c.ToSequence,
		)
		if err != nil {
			return nil, err
		}
		currencyMovements, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (line, error) {
			var (
				m                    lineMovement
				amount, balanceAfter int64
			)
			err := row.Scan(&m.Sequence, &amount, &balanceAfter, &m.Kind, &m.Reference)
			m.Currency, m.Amount, m.BalanceAfter = c.Currency, strconv.FormatInt(amount, 10), strconv.FormatInt(balanceAfter, 10)
			return line{Kind: "MOVEMENT", Movement: &m}, err
		})
		if err != nil {
			return nil, err
		}
		movements = append(movements, currencyMovements...)
	}
	return movements, nil
}

// statementOutcomes lists the terminal outcomes no earlier statement of the chain has reported.
// The recursive CTE walks previous_id back to the first statement of the chain, and a statement
// taken after nothing starts a new chain, so it reports every outcome again.
func statementOutcomes(ctx context.Context, tx pgx.Tx, account string, after *uuid.UUID) ([]line, error) {
	rows, err := tx.Query(ctx, `
		with recursive chain as (
			select id, previous_id from statement where id = $2
			union all
			select s.id, s.previous_id from statement s join chain c on s.id = c.previous_id
		)
		select r.kind, r.reference, r.state,
		       case r.kind when 'TRADE' then t.buy_currency else p.currency end,
		       case r.kind when 'TRADE' then case when r.state = 'FILLED' then t.buy_amount end else p.amount end,
		       p.failure_code,
		       coalesce(t.executed_at, p.finalized_at, r.updated_at)
		from operation_reference r
		left join trade t on r.kind = 'TRADE' and t.provider_account_id = r.provider_account_id and t.reference = r.reference
		left join payout p on r.kind = 'PAYOUT' and p.provider_account_id = r.provider_account_id and p.reference = r.reference
		where r.provider_account_id = $1
		  and r.state in ('FILLED', 'REJECTED', 'CLOSED', 'COMPLETED', 'FAILED', 'CANCELLED')
		  and not exists (
		      select from statement_line l
		      where l.statement_id in (select id from chain) and l.kind = 'OUTCOME'
		        and l.operation = r.kind and l.reference = r.reference
		  )
		order by 7, r.kind, r.reference`,
		account, after,
	)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (line, error) {
		var (
			o           lineOutcome
			currency    *string
			amount      *int64
			failureCode *string
		)
		err := row.Scan(&o.Operation, &o.Reference, &o.Status, &currency, &amount, &failureCode, &o.FinalizedAt)
		if currency != nil && amount != nil {
			o.Currency, o.Amount = *currency, strconv.FormatInt(*amount, 10)
		}
		if failureCode != nil {
			o.FailureCode = *failureCode
		}
		o.FinalizedAt = o.FinalizedAt.UTC()
		return line{Kind: "OUTCOME", Outcome: &o}, err
	})
}

func (s *Service) StatementPage(ctx context.Context, account string, id uuid.UUID, cursor int) ([]json.RawMessage, int, error) {
	var known bool
	if err := s.pool.QueryRow(ctx, "select exists (select from statement where id = $1 and provider_account_id = $2)", id, account).Scan(&known); err != nil {
		return nil, 0, fmt.Errorf("statement page: %w", err)
	}
	if !known {
		return nil, 0, ErrNotFound
	}
	rows, err := s.pool.Query(ctx, `
		select line_no, body from statement_line where statement_id = $1 and line_no > $2 order by line_no limit $3`,
		id, cursor, PageSize,
	)
	if err != nil {
		return nil, 0, fmt.Errorf("statement page: %w", err)
	}
	var (
		lines []json.RawMessage
		last  int
	)
	for rows.Next() {
		var body string
		if err := rows.Scan(&last, &body); err != nil {
			rows.Close()
			return nil, 0, fmt.Errorf("statement page: %w", err)
		}
		lines = append(lines, json.RawMessage(body))
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("statement page: %w", err)
	}
	if len(lines) < PageSize {
		last = 0
	}
	return lines, last, nil
}

func (s *Service) StatementLine(ctx context.Context, account string, id uuid.UUID, lineNo int) (json.RawMessage, error) {
	var body string
	err := s.pool.QueryRow(ctx, `
		select l.body from statement_line l join statement s on s.id = l.statement_id
		where s.id = $1 and s.provider_account_id = $2 and l.line_no = $3`,
		id, account, lineNo,
	).Scan(&body)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("statement line: %w", err)
	}
	return json.RawMessage(body), nil
}
