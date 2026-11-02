package api_test

import (
	"context"
	"testing"
	"time"

	"github.com/xJohnNascimento/ditto/internal/api"
)

func (h *harness) drain() {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := h.service.DeliverWebhooks(context.Background(), h.srv.Client(), 100); err != nil {
			h.t.Fatal(err)
		}
		var pending int
		err := h.db.QueryRow(context.Background(), "select count(*) from webhook_delivery where delivered_at is null and gave_up_at is null").Scan(&pending)
		if err != nil {
			h.t.Fatal(err)
		}
		if pending == 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	h.t.Fatal("webhooks still pending after 5 s")
}

func statuses(events []delivered) []string {
	out := make([]string, len(events))
	for i, e := range events {
		out[i] = e.Event.Data.Status
	}
	return out
}

func TestPayoutEventsAreSignedAndRetriedUnderOneID(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fund("corridor", "USD", "10000", "f-1")
	h.receiver.failNext(2)

	h.call("POST", "/v1/payouts", corridorKey, payoutRequest("p-1", "USD", "1000"))
	h.drain()
	h.execute()
	h.drain()
	h.execute()
	h.drain()

	events := h.receiver.events("p-1")
	if got := statuses(events); len(got) != 3 || got[0] != "ACCEPTED" || got[1] != "PROCESSING" || got[2] != "COMPLETED" {
		t.Fatalf("delivered %v, want ACCEPTED, PROCESSING, COMPLETED", got)
	}
	for i, e := range events {
		if !e.Valid || e.ID != e.Event.EventID || e.Event.Type != "payout.status_changed" || e.Event.Data.Sequence != i+1 {
			t.Fatalf("event %d: %+v", i, e)
		}
	}
	if attempts := h.receiver.attemptsOf(events[0].ID); attempts != 3 {
		t.Fatalf("the first event was attempted %d times under one id, want 3", attempts)
	}
}

func TestWebhookFaultsChangeOnlyTheDelivery(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fund("corridor", "USD", "10000", "f-1")
	h.arm(api.FaultWEBHOOKDUPLICATE, api.ScenarioTargetKindPayout, "p-dup", nil)
	h.arm(api.FaultWEBHOOKOMIT, api.ScenarioTargetKindPayout, "p-omit", nil)
	h.arm(api.FaultWEBHOOKREORDER, api.ScenarioTargetKindPayout, "p-reorder", &api.ScenarioArgs{DelayMs: intPtr(300)})
	h.arm(api.FaultFALSEFAILEDHINT, api.ScenarioTargetKindPayout, "p-hint", nil)
	for _, reference := range []string{"p-dup", "p-omit", "p-reorder", "p-hint"} {
		h.call("POST", "/v1/payouts", corridorKey, payoutRequest(reference, "USD", "1000"))
	}
	h.execute()
	h.drain()
	if p := h.payout("p-hint"); p.Status != api.PayoutStatusPROCESSING {
		t.Fatalf("the hinted payout is %s", p.Status)
	}
	hint := statuses(h.receiver.events("p-hint"))
	h.execute()
	h.drain()

	if dup := h.receiver.events("p-dup"); len(dup) != 5 || dup[0].ID != dup[1].ID || dup[1].ID != dup[2].ID {
		t.Fatalf("duplicates: %v", statuses(dup))
	}
	if got := statuses(h.receiver.events("p-omit")); len(got) != 2 || got[1] != "PROCESSING" {
		t.Fatalf("omitted terminal event: delivered %v", got)
	}
	if got := statuses(h.receiver.events("p-reorder")); len(got) != 3 || got[0] != "PROCESSING" || got[1] != "ACCEPTED" {
		t.Fatalf("reordered: delivered %v", got)
	}
	if len(hint) != 3 || hint[1] != "PROCESSING" || hint[2] != "FAILED" {
		t.Fatalf("false failed hint: delivered %v", hint)
	}
}

func TestDeliveryGivesUpAfterItsWindow(t *testing.T) {
	t.Parallel()
	opts := defaultOptions()
	opts.WebhookWindow = time.Millisecond
	h := newHarnessWith(t, opts)
	h.fund("corridor", "USD", "10000", "f-1")
	h.receiver.failNext(1000)

	h.call("POST", "/v1/payouts", corridorKey, payoutRequest("p-1", "USD", "1000"))
	eventually(t, "the retry window to close", func() bool {
		var closed bool
		err := h.db.QueryRow(context.Background(), "select bool_and(now() > created_at + interval '1 millisecond') from webhook_event").Scan(&closed)
		if err != nil {
			t.Fatal(err)
		}
		return closed
	})
	h.drain()
	var attempts, gaveUp int
	err := h.db.QueryRow(context.Background(), "select sum(attempts), count(gave_up_at) from webhook_delivery").Scan(&attempts, &gaveUp)
	if err != nil || attempts != 1 || gaveUp != 1 {
		t.Fatalf("%d attempts, %d given up (%v); want one attempt and a give-up", attempts, gaveUp, err)
	}
}
