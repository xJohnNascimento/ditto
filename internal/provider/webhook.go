package provider

import (
	"bytes"
	"cmp"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const (
	maxBackoff     = time.Hour
	reorderDefault = 5 * time.Second

	defaultSendTimeout = 30 * time.Second
	leaseMargin        = 10 * time.Second

	// AC04 (closed account) is the failure Corridor maps specially, so a hint armed without a code
	// still exercises that path.
	defaultHintFailureCode = "AC04"
)

// payoutHints maps the status being announced to the fault that follows it with a contradicting
// event. FALSE_FAILED_HINT lies about a payout that is still processing, CONTRADICTORY_FINAL_HINT
// about one that already reached a final status; the payout itself never changes.
var payoutHints = map[PayoutStatus]string{
	PayoutStatusProcessing: FalseFailedHint,
	PayoutStatusCompleted:  ContradictoryFinalHint,
	PayoutStatusFailed:     ContradictoryFinalHint,
	PayoutStatusCancelled:  ContradictoryFinalHint,
}

type Endpoint struct {
	URL     string
	Secrets [][]byte
}

type eventData struct {
	PayoutID    string       `json:"payout_id,omitempty"`
	Reference   string       `json:"reference"`
	Status      PayoutStatus `json:"status"`
	Sequence    int          `json:"sequence"`
	Currency    string       `json:"currency,omitempty"`
	Amount      string       `json:"amount,omitempty"`
	FailureCode string       `json:"failure_code,omitempty"`
}

type event struct {
	ID   string    `json:"event_id"`
	Type string    `json:"type"`
	Data eventData `json:"data"`
}

// ParseWebhookSecrets reads Standard Webhooks secrets ("whsec_" and base64), space-separated so a
// rotation can sign with the old and the new secret at once.
func ParseWebhookSecrets(value string) ([][]byte, error) {
	var keys [][]byte
	for _, secret := range strings.Fields(value) {
		encoded, ok := strings.CutPrefix(secret, "whsec_")
		key, err := base64.StdEncoding.DecodeString(encoded)
		if !ok || err != nil || len(key) < 24 {
			return nil, errors.New("webhook secrets must be whsec_ followed by at least 24 base64-encoded bytes")
		}
		keys = append(keys, key)
	}
	if len(keys) == 0 {
		return nil, errors.New("no webhook secret")
	}
	return keys, nil
}

func Sign(secrets [][]byte, id string, timestamp int64, body []byte) string {
	signatures := make([]string, len(secrets))
	for i, key := range secrets {
		mac := hmac.New(sha256.New, key)
		_, _ = fmt.Fprintf(mac, "%s.%d.", id, timestamp)
		mac.Write(body)
		signatures[i] = "v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil))
	}
	return strings.Join(signatures, " ")
}

// emitPayoutEvent records the transition's event and its deliveries in the transition's own
// transaction, applying any webhook fault armed on the payout.
func (s *Service) emitPayoutEvent(ctx context.Context, tx pgx.Tx, account string, p Payout) error {
	if err := s.writeEvent(ctx, tx, account, p, payoutEventData(p)); err != nil {
		return err
	}
	fault, ok := payoutHints[p.Status]
	if !ok {
		return nil
	}
	hint, err := consume(ctx, tx, targetPayout, p.Reference, fault)
	if err != nil || hint == nil {
		return err
	}
	return s.writeEvent(ctx, tx, account, p, contradictingEvent(p, hint))
}

// contradictingEvent is the event a hint fault adds after the payout's real one. It is always a
// final one that disagrees with the truth: FAILED for a payout that is processing or
// completed, COMPLETED for one that failed or was cancelled.
func contradictingEvent(p Payout, hint *Arm) eventData {
	data := payoutEventData(p)
	data.Sequence = finalSequence
	if p.Status == PayoutStatusFailed || p.Status == PayoutStatusCancelled {
		data.Status, data.FailureCode = PayoutStatusCompleted, ""
		return data
	}
	data.Status, data.FailureCode = PayoutStatusFailed, cmp.Or(hint.Args.Code, defaultHintFailureCode)
	return data
}

func (s *Service) writeEvent(ctx context.Context, tx pgx.Tx, account string, p Payout, data eventData) error {
	id, err := uuid.NewV7()
	if err != nil {
		return err
	}
	e := event{ID: "evt_" + id.String(), Type: "payout.status_changed", Data: data}
	body, err := json.Marshal(e)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
		insert into webhook_event (id, provider_account_id, reference, status, sequence, body)
		values ($1, $2, $3, $4, $5, $6)`,
		e.ID, account, p.Reference, data.Status, data.Sequence, string(body),
	)
	if err != nil {
		return err
	}
	if _, ok := s.endpoints[account]; !ok {
		return nil
	}

	copies, delay := 1, time.Duration(0)
	faults := []string{WebhookDuplicate, WebhookReorder}
	if data.Sequence == finalSequence {
		faults = append(faults, WebhookOmit)
	}
	arm, err := consume(ctx, tx, targetPayout, p.Reference, faults...)
	if err != nil {
		return err
	}
	if arm != nil {
		switch arm.Fault {
		case WebhookOmit:
			return nil
		case WebhookDuplicate:
			copies = 3
		case WebhookReorder:
			delay = reorderDefault
			if arm.Args.DelayMS > 0 {
				delay = time.Duration(arm.Args.DelayMS) * time.Millisecond
			}
		}
	}
	for n := 1; n <= copies; n++ {
		_, err := tx.Exec(ctx, `
			insert into webhook_delivery (event_id, copy, next_attempt_at) values ($1, $2, now() + make_interval(secs => $3))`,
			e.ID, n, delay.Seconds(),
		)
		if err != nil {
			return err
		}
	}
	return nil
}

func payoutEventData(p Payout) eventData {
	data := eventData{Reference: p.Reference, Status: p.Status, Sequence: p.Sequence, FailureCode: p.FailureCode}
	if p.HasTerms {
		data.PayoutID, data.Currency, data.Amount = p.ID.String(), p.Currency, strconv.FormatInt(p.Amount, 10)
	}
	return data
}

func (s *Service) RunWebhooks(ctx context.Context, client *http.Client, every time.Duration) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := s.DeliverWebhooks(ctx, client, 50); err != nil && ctx.Err() == nil {
				slog.ErrorContext(ctx, "deliver webhooks", "error", err)
			}
		}
	}
}

type delivery struct {
	dueAt     time.Time
	eventID   string
	copy      int
	attempts  int
	account   string
	body      string
	createdAt time.Time
}

// retryBackoff doubles base with every attempt up to maxBackoff. It stops doubling at the cap, so
// a large attempt count never overflows the duration the way a plain shift does.
func retryBackoff(base time.Duration, attempts int) time.Duration {
	if base <= 0 {
		return 0
	}
	backoff := base
	for attempt := 1; attempt < attempts && backoff < maxBackoff; attempt++ {
		backoff *= 2
	}
	return min(backoff, maxBackoff)
}

// leaseFor covers a whole batch: its deliveries are sent one after another, each for at most the
// client's timeout, and a shorter lease would let another instance take a delivery still queued
// behind them in this batch.
func leaseFor(client *http.Client, limit int) time.Duration {
	return cmp.Or(client.Timeout, defaultSendTimeout)*time.Duration(limit) + leaseMargin
}

// DeliverWebhooks leases due deliveries, so instances never send the same copy at once, and sends
// each with fresh Standard Webhooks headers. A failure backs off exponentially until the retry
// window closes; a restart resumes whatever is still pending, and a claim abandoned by a crashed
// instance expires with its lease.
func (s *Service) DeliverWebhooks(ctx context.Context, client *http.Client, limit int) (int, error) {
	rows, err := s.pool.Query(ctx, `
		update webhook_delivery d
		set lease_until = now() + make_interval(secs => $2), attempts = d.attempts + 1
		from webhook_event e
		where e.id = d.event_id
		  and (d.event_id, d.copy) in (
		      select event_id, copy from webhook_delivery
		      where delivered_at is null and gave_up_at is null and next_attempt_at <= now()
		        and (lease_until is null or lease_until < now())
		      order by next_attempt_at
		      limit $1
		      for update skip locked
		  )
		returning d.next_attempt_at, d.event_id, d.copy, d.attempts, e.provider_account_id, e.body, e.created_at`,
		limit, leaseFor(client, limit).Seconds(),
	)
	if err != nil {
		return 0, fmt.Errorf("lease webhook deliveries: %w", err)
	}
	due, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (delivery, error) {
		var d delivery
		return d, row.Scan(&d.dueAt, &d.eventID, &d.copy, &d.attempts, &d.account, &d.body, &d.createdAt)
	})
	if err != nil {
		return 0, fmt.Errorf("lease webhook deliveries: %w", err)
	}
	slices.SortFunc(due, func(a, b delivery) int {
		return cmp.Or(a.dueAt.Compare(b.dueAt), cmp.Compare(a.eventID, b.eventID), cmp.Compare(a.copy, b.copy))
	})
	delivered := 0
	for _, d := range due {
		sendErr := s.send(ctx, client, d)
		if sendErr == nil {
			delivered++
			_, err = s.pool.Exec(ctx, "update webhook_delivery set delivered_at = now(), lease_until = null, last_error = null where event_id = $1 and copy = $2", d.eventID, d.copy)
		} else {
			_, err = s.pool.Exec(ctx, `
				update webhook_delivery
				set next_attempt_at = now() + make_interval(secs => $3), lease_until = null, last_error = $4,
				    gave_up_at = case when now() + make_interval(secs => $3) > $5::timestamptz + make_interval(secs => $6) then now() end
				where event_id = $1 and copy = $2`,
				d.eventID, d.copy, retryBackoff(s.opts.WebhookBackoff, d.attempts).Seconds(), sendErr.Error(), d.createdAt, s.opts.WebhookWindow.Seconds(),
			)
		}
		if err != nil {
			return delivered, fmt.Errorf("record webhook delivery %s/%d: %w", d.eventID, d.copy, err)
		}
	}
	return delivered, nil
}

func (s *Service) send(ctx context.Context, client *http.Client, d delivery) error {
	endpoint, ok := s.endpoints[d.account]
	if !ok {
		return errors.New("no endpoint configured")
	}
	timestamp := time.Now().Unix()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.URL, bytes.NewReader([]byte(d.body)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("webhook-id", d.eventID)
	req.Header.Set("webhook-timestamp", strconv.FormatInt(timestamp, 10))
	req.Header.Set("webhook-signature", Sign(endpoint.Secrets, d.eventID, timestamp, []byte(d.body)))
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	// Draining lets the transport reuse the connection; the cap bounds what a misbehaving endpoint can make us read.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	_ = resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("endpoint answered %s", resp.Status)
	}
	return nil
}
