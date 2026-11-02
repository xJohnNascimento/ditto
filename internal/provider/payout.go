package provider

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type Beneficiary struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Account  string `json:"account"`
	BankCode string `json:"bank_code"`
	Country  string `json:"country"`
}

type PayoutRequest struct {
	Reference      string      `json:"-"`
	Currency       string      `json:"currency"`
	Amount         int64       `json:"amount"`
	Beneficiary    Beneficiary `json:"beneficiary"`
	RemittanceInfo string      `json:"remittance_info"`
}

const failedStepCooldown = 30 * time.Second

type PayoutStatus string

const (
	PayoutStatusAccepted   PayoutStatus = "ACCEPTED"
	PayoutStatusRejected   PayoutStatus = "REJECTED"
	PayoutStatusProcessing PayoutStatus = "PROCESSING"
	PayoutStatusCompleted  PayoutStatus = "COMPLETED"
	PayoutStatusFailed     PayoutStatus = "FAILED"
	PayoutStatusCancelled  PayoutStatus = "CANCELLED"
)

type Payout struct {
	lagging     bool
	Reference   string
	Status      PayoutStatus
	DropReply   bool
	Faults      []string
	Sequence    int
	HasTerms    bool
	ID          uuid.UUID
	Currency    string
	Amount      int64
	FailureCode string
}

const finalSequence = 3

var sequenceOf = map[PayoutStatus]int{
	PayoutStatusAccepted:   1,
	PayoutStatusRejected:   1,
	PayoutStatusProcessing: 2,
	PayoutStatusCompleted:  finalSequence,
	PayoutStatusFailed:     finalSequence,
	PayoutStatusCancelled:  finalSequence,
}

// Ten seconds is long enough for a client polling every few seconds to still read PROCESSING after
// the statement shows the completion, and short enough for a test to wait it out.
const statusLag = 10 * time.Second

func (s *Service) CreatePayout(ctx context.Context, account string, req PayoutRequest) (Payout, bool, error) {
	hash := requestHash(req)
	var (
		result  Payout
		created bool
	)
	delayed, err := s.delayBeforeExecution(ctx, targetPayout, req.Reference)
	if err != nil {
		return Payout{}, false, fmt.Errorf("create payout: %w", err)
	}
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		c, err := claimReference(ctx, tx, account, kindPayout, req.Reference, hash)
		if err != nil {
			return err
		}
		if !c.created {
			if created, err = replayOrConflict(ctx, tx, account, kindPayout, req.Reference, c, hash); err != nil {
				return err
			}
			result, err = payoutByReference(ctx, tx, account, req.Reference)
			result = result.observed()
			return err
		}
		result, err = s.acceptPayout(ctx, tx, account, req)
		created = err == nil
		return err
	})
	if err != nil {
		return Payout{}, false, fmt.Errorf("create payout: %w", err)
	}
	if delayed {
		result.Faults = append(result.Faults, DelayBeforeExecution)
	}
	return result, created, nil
}

func (s *Service) acceptPayout(ctx context.Context, tx pgx.Tx, account string, req PayoutRequest) (Payout, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return Payout{}, err
	}
	p := Payout{Reference: req.Reference, Status: PayoutStatusAccepted, HasTerms: true, ID: id, Currency: req.Currency, Amount: req.Amount}
	_, sufficient, err := move(ctx, tx, account, req.Reference, id, 1, []leg{{currency: req.Currency, amount: -req.Amount, kind: "PAYOUT_DEBIT"}})
	if err != nil {
		return Payout{}, err
	}
	if !sufficient {
		p.Status, p.FailureCode = PayoutStatusRejected, "AM04"
	}
	p.Sequence = sequenceOf[p.Status]
	_, err = tx.Exec(ctx, `
		insert into payout (id, provider_account_id, reference, currency, amount, beneficiary, remittance_info, failure_code, due_at, finalized_at)
		values ($1, $2, $3, $4, $5, $6, nullif($7, ''), nullif($8, ''), now() + make_interval(secs => $9),
		        case when $8 <> '' then now() end)`,
		id, account, req.Reference, req.Currency, req.Amount, req.Beneficiary, req.RemittanceInfo, p.FailureCode, s.opts.PayoutStep.Seconds(),
	)
	if err != nil {
		return Payout{}, err
	}
	if err := setState(ctx, tx, account, kindPayout, req.Reference, p.Status); err != nil {
		return Payout{}, err
	}
	if err := s.emitPayoutEvent(ctx, tx, account, p); err != nil {
		return Payout{}, err
	}
	drop, err := consume(ctx, tx, targetPayout, req.Reference, PayoutCommitDropReply)
	if drop != nil {
		p.DropReply, p.Faults = true, append(p.Faults, drop.Fault)
	}
	return p, err
}

func (s *Service) Payout(ctx context.Context, account, reference string) (Payout, error) {
	p, err := payoutByReference(ctx, s.pool, account, reference)
	if errors.Is(err, pgx.ErrNoRows) {
		return Payout{}, ErrNotFound
	}
	if err != nil {
		return Payout{}, fmt.Errorf("get payout: %w", err)
	}
	return p.observed(), nil
}

// observed is what reads answer: a completion under STATUS_API_LAG still reads as PROCESSING until
// its lag ends, although statements already show it.
func (p Payout) observed() Payout {
	if p.lagging {
		p.Status, p.Sequence = PayoutStatusProcessing, sequenceOf[PayoutStatusProcessing]
	}
	return p
}

func payoutByReference(ctx context.Context, q querier, account, reference string) (Payout, error) {
	var (
		p           Payout
		id          *uuid.UUID
		currency    *string
		amount      *int64
		failureCode *string
	)
	err := q.QueryRow(ctx, `
		select r.reference, r.state, p.id, p.currency, p.amount, p.failure_code, coalesce(p.lagged_until > now(), false)
		from operation_reference r
		left join payout p on p.provider_account_id = r.provider_account_id and p.reference = r.reference
		where r.provider_account_id = $1 and r.kind = 'PAYOUT' and r.reference = $2`,
		account, reference,
	).Scan(&p.Reference, &p.Status, &id, &currency, &amount, &failureCode, &p.lagging)
	if err != nil {
		return p, err
	}
	p.Sequence = sequenceOf[p.Status]
	if failureCode != nil {
		p.FailureCode = *failureCode
	}
	if id != nil {
		p.HasTerms, p.ID, p.Currency, p.Amount = true, *id, *currency, *amount
	}
	return p, nil
}

// RunScheduler advances due payouts until ctx ends. Every instance may run it: each step locks its
// reference with SKIP LOCKED, so a payout being cancelled or advanced elsewhere is left alone.
func (s *Service) RunScheduler(ctx context.Context, every time.Duration) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := s.ExecuteDuePayouts(ctx, 100); err != nil && ctx.Err() == nil {
				slog.ErrorContext(ctx, "execute due payouts", "error", err)
			}
		}
	}
}

// ExecuteDuePayouts advances up to limit due payouts. One payout failing must not hold back the
// others, so it logs the error, goes on and returns every error joined.
func (s *Service) ExecuteDuePayouts(ctx context.Context, limit int) (int, error) {
	rows, err := s.pool.Query(ctx, `
		select provider_account_id, reference from payout
		where finalized_at is null and due_at <= now()
		order by due_at
		limit $1`,
		limit,
	)
	if err != nil {
		return 0, fmt.Errorf("list due payouts: %w", err)
	}
	type due struct{ account, reference string }
	candidates, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (due, error) {
		var d due
		return d, row.Scan(&d.account, &d.reference)
	})
	if err != nil {
		return 0, fmt.Errorf("list due payouts: %w", err)
	}
	advanced := 0
	var failures []error
	for _, d := range candidates {
		if ctx.Err() != nil {
			failures = append(failures, ctx.Err())
			break
		}
		moved, err := s.advancePayout(ctx, d.account, d.reference)
		if err != nil {
			slog.ErrorContext(ctx, "advance payout", "account", d.account, "reference", d.reference, "error", err)
			failures = append(failures, fmt.Errorf("advance payout %s/%s: %w", d.account, d.reference, err))
			continue
		}
		if moved {
			advanced++
		}
	}
	return advanced, errors.Join(failures...)
}

func (s *Service) advancePayout(ctx context.Context, account, reference string) (bool, error) {
	moved := false
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var (
			state PayoutStatus
			due   bool
		)
		err := tx.QueryRow(ctx, `
			select r.state, p.due_at <= now() and p.finalized_at is null
			from operation_reference r
			join payout p on p.provider_account_id = r.provider_account_id and p.reference = r.reference
			where r.provider_account_id = $1 and r.kind = 'PAYOUT' and r.reference = $2
			for update of r skip locked`,
			account, reference,
		).Scan(&state, &due)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if !due {
			return nil
		}
		p, err := payoutByReference(ctx, tx, account, reference)
		if err != nil {
			return err
		}
		switch state {
		case PayoutStatusAccepted:
			moved = true
			_, err := tx.Exec(ctx, "update payout set due_at = now() + make_interval(secs => $3) where provider_account_id = $1 and reference = $2",
				account, reference, s.opts.PayoutStep.Seconds())
			if err != nil {
				return err
			}
			if err := setState(ctx, tx, account, kindPayout, reference, PayoutStatusProcessing); err != nil {
				return err
			}
			p.Status, p.Sequence = PayoutStatusProcessing, sequenceOf[PayoutStatusProcessing]
			return s.emitPayoutEvent(ctx, tx, account, p)
		case PayoutStatusProcessing:
			moved, err = s.executePayout(ctx, tx, account, p)
			return err
		case PayoutStatusRejected, PayoutStatusCompleted, PayoutStatusFailed, PayoutStatusCancelled:
			return nil
		}
		return nil
	})
	if err != nil {
		// A step that keeps failing would otherwise be due again on the next tick and, with enough
		// of them, fill the page and starve the healthy payouts. The rollback restored due_at, so the
		// cooldown has to be a separate statement.
		_, cooldownErr := s.pool.Exec(ctx,
			"update payout set due_at = now() + make_interval(secs => $3) where provider_account_id = $1 and reference = $2 and finalized_at is null",
			account, reference, failedStepCooldown.Seconds())
		return false, errors.Join(err, cooldownErr)
	}
	return moved, nil
}

// executePayout is the rail's decision for a processing payout, and reports whether it moved. A
// stuck payout is parked until a cancel; a failure is refunded in the same transaction; a lagged
// completion delays its event too.
func (s *Service) executePayout(ctx context.Context, tx pgx.Tx, account string, p Payout) (bool, error) {
	arm, err := consume(ctx, tx, targetPayout, p.Reference, PayoutStuckCancellable, PayoutFail, StatusAPILag)
	if err != nil {
		return false, err
	}
	switch {
	case arm != nil && arm.Fault == PayoutStuckCancellable:
		_, err := tx.Exec(ctx, "update payout set due_at = 'infinity' where provider_account_id = $1 and reference = $2", account, p.Reference)
		return false, err
	case arm != nil && arm.Fault == PayoutFail:
		if err := refund(ctx, tx, account, p); err != nil {
			return false, err
		}
		return true, s.finishPayout(ctx, tx, account, p, PayoutStatusFailed, arm.Args.Code)
	}
	if err := s.finishPayout(ctx, tx, account, p, PayoutStatusCompleted, ""); err != nil {
		return false, err
	}
	if arm == nil {
		return true, nil
	}
	return true, lagCompletion(ctx, tx, account, p.Reference, arm)
}

func lagCompletion(ctx context.Context, tx pgx.Tx, account, reference string, arm *Arm) error {
	lag := statusLag
	if arm.Args.DurationMS > 0 {
		lag = time.Duration(arm.Args.DurationMS) * time.Millisecond
	}
	_, err := tx.Exec(ctx, "update payout set lagged_until = now() + make_interval(secs => $3) where provider_account_id = $1 and reference = $2",
		account, reference, lag.Seconds())
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
		update webhook_delivery set next_attempt_at = greatest(next_attempt_at, now() + make_interval(secs => $3))
		where event_id in (select id from webhook_event where provider_account_id = $1 and reference = $2 and status = 'COMPLETED')`,
		account, reference, lag.Seconds(),
	)
	return err
}
