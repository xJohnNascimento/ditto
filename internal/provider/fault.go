package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const (
	TradeReject            = "TRADE_REJECT"
	TradeCommitDropReply   = "TRADE_COMMIT_DROP_REPLY"
	FXFillDifference       = "FX_FILL_DIFFERENCE"
	DelayBeforeExecution   = "DELAY_BEFORE_EXECUTION"
	PayoutFail             = "PAYOUT_FAIL"
	PayoutCommitDropReply  = "PAYOUT_COMMIT_DROP_REPLY"
	PayoutStuckCancellable = "PAYOUT_STUCK_CANCELLABLE"
	StatusAPILag           = "STATUS_API_LAG"
	WebhookDuplicate       = "WEBHOOK_DUPLICATE"
	WebhookReorder         = "WEBHOOK_REORDER"
	WebhookOmit            = "WEBHOOK_OMIT"
	FalseFailedHint        = "FALSE_FAILED_HINT"
	ContradictoryFinalHint = "CONTRADICTORY_FINAL_HINT"
	RateLimit              = "RATE_LIMIT"
)

const (
	targetQuote   = "quote"
	targetPayout  = "payout"
	targetAccount = "account"
)

var faultTargets = map[string][]string{
	TradeReject:            {targetQuote},
	TradeCommitDropReply:   {targetQuote},
	FXFillDifference:       {targetQuote},
	DelayBeforeExecution:   {targetQuote, targetPayout},
	PayoutFail:             {targetPayout},
	PayoutCommitDropReply:  {targetPayout},
	PayoutStuckCancellable: {targetPayout},
	StatusAPILag:           {targetPayout},
	WebhookDuplicate:       {targetPayout},
	WebhookReorder:         {targetPayout},
	WebhookOmit:            {targetPayout},
	FalseFailedHint:        {targetPayout},
	ContradictoryFinalHint: {targetPayout},
	RateLimit:              {targetAccount},
}

var failureCode = regexp.MustCompile(`^[A-Z]{2}[0-9]{2}$`)

type FaultArgs struct {
	Delta      int64  `json:"delta,omitempty"`
	Code       string `json:"code,omitempty"`
	DelayMS    int64  `json:"delay_ms,omitempty"`
	DurationMS int64  `json:"duration_ms,omitempty"`
}

type Arm struct {
	ID            uuid.UUID
	Fault         string
	TargetKind    string
	TargetValue   string
	Args          FaultArgs
	RemainingUses int
	Consumptions  []Consumption
}

type Consumption struct {
	RequestID  string
	ConsumedAt time.Time
}

func (s *Service) ArmFault(ctx context.Context, fault, kind, value string, args FaultArgs, uses int) (Arm, error) {
	if !slices.Contains(faultTargets[fault], kind) {
		return Arm{}, invalid("%s cannot target a %s", fault, kind)
	}
	switch {
	case fault == FXFillDifference && args.Delta == 0:
		return Arm{}, invalid("%s needs a non-zero delta", fault)
	case fault == PayoutFail && !failureCode.MatchString(args.Code):
		return Arm{}, invalid("%s needs an ISO 20022 code such as AC04", fault)
	case fault == DelayBeforeExecution && args.DelayMS <= 0:
		return Arm{}, invalid("%s needs a positive delay_ms", fault)
	case fault == RateLimit && args.DurationMS <= 0:
		return Arm{}, invalid("%s needs a positive duration_ms", fault)
	}
	id, err := uuid.NewV7()
	if err != nil {
		return Arm{}, fmt.Errorf("arm fault: %w", err)
	}
	_, err = s.pool.Exec(ctx, `
		insert into scenario_arm (id, fault, target_kind, target_value, args, remaining_uses)
		values ($1, $2, $3, $4, $5, $6)`,
		id, fault, kind, value, args, uses,
	)
	if err != nil {
		return Arm{}, fmt.Errorf("arm fault: %w", err)
	}
	return Arm{ID: id, Fault: fault, TargetKind: kind, TargetValue: value, Args: args, RemainingUses: uses}, nil
}

func (s *Service) Arms(ctx context.Context) ([]Arm, error) {
	rows, err := s.pool.Query(ctx, `
		select a.id, a.fault, a.target_kind, a.target_value, a.args, a.remaining_uses,
		       coalesce(json_agg(json_build_object('request_id', c.request_id, 'consumed_at', c.consumed_at)
		                order by c.id) filter (where c.arm_id is not null), '[]')
		from scenario_arm a
		left join scenario_consumption c on c.arm_id = a.id
		group by a.id
		order by a.created_at`)
	if err != nil {
		return nil, fmt.Errorf("list arms: %w", err)
	}
	arms, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Arm, error) {
		var (
			a            Arm
			consumptions []byte
		)
		if err := row.Scan(&a.ID, &a.Fault, &a.TargetKind, &a.TargetValue, &a.Args, &a.RemainingUses, &consumptions); err != nil {
			return Arm{}, err
		}
		var raw []struct {
			RequestID  string    `json:"request_id"`
			ConsumedAt time.Time `json:"consumed_at"`
		}
		if err := json.Unmarshal(consumptions, &raw); err != nil {
			return Arm{}, err
		}
		for _, c := range raw {
			a.Consumptions = append(a.Consumptions, Consumption(c))
		}
		return a, nil
	})
	if err != nil {
		return nil, fmt.Errorf("list arms: %w", err)
	}
	return arms, nil
}

type requestIDKey struct{}

func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, id)
}

func requestID(ctx context.Context) string {
	if id, ok := ctx.Value(requestIDKey{}).(string); ok {
		return id
	}
	return "scheduler"
}

// consume takes one use of the oldest armed fault among faults on the target and records which
// request took it, in the caller's transaction: if the effect rolls back, so does the consumption.
func consume(ctx context.Context, tx pgx.Tx, kind, value string, faults ...string) (*Arm, error) {
	var a Arm
	err := tx.QueryRow(ctx, `
		update scenario_arm set remaining_uses = remaining_uses - 1
		where id = (
			select id from scenario_arm
			where target_kind = $1 and target_value = $2 and fault = any($3) and remaining_uses > 0
			order by created_at
			limit 1
			for update skip locked
		)
		returning id, fault, args`,
		kind, value, faults,
	).Scan(&a.ID, &a.Fault, &a.Args)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	_, err = tx.Exec(ctx, "insert into scenario_consumption (arm_id, request_id) values ($1, $2)", a.ID, requestID(ctx))
	return &a, err
}

// delayBeforeExecution consumes a delay in its own transaction and sleeps before the caller takes
// the reference lock, so a close or cancel can win the race in between.
func (s *Service) delayBeforeExecution(ctx context.Context, kind, value string) (bool, error) {
	var arm *Arm
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		arm, err = consume(ctx, tx, kind, value, DelayBeforeExecution)
		return err
	})
	if err != nil || arm == nil {
		return false, err
	}
	select {
	case <-ctx.Done():
		return true, ctx.Err()
	case <-time.After(time.Duration(arm.Args.DelayMS) * time.Millisecond):
		return true, nil
	}
}

type CatalogueEntry struct {
	Fault   string
	Targets []string
}

func Catalogue() []CatalogueEntry {
	entries := make([]CatalogueEntry, 0, len(faultTargets))
	for fault, targets := range faultTargets {
		entries = append(entries, CatalogueEntry{Fault: fault, Targets: targets})
	}
	slices.SortFunc(entries, func(a, b CatalogueEntry) int { return strings.Compare(a.Fault, b.Fault) })
	return entries
}
