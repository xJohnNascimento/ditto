package api_test

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"

	"github.com/xJohnNascimento/ditto/internal/api"
)

func (h *harness) fence(path string) api.FenceResultStatus {
	h.t.Helper()
	var result api.FenceResult
	resp := h.call(http.MethodPost, path, corridorKey, nil)
	if resp.status != http.StatusOK {
		h.t.Fatalf("%s: %d %s", path, resp.status, resp.body)
	}
	resp.decode(h.t, &result)
	return result.Status
}

func (h *harness) arm(fault api.Fault, kind api.ScenarioTargetKind, value string, args *api.ScenarioArgs) api.ScenarioArm {
	h.t.Helper()
	return h.armTimes(fault, kind, value, args, 1)
}

func (h *harness) armTimes(fault api.Fault, kind api.ScenarioTargetKind, value string, args *api.ScenarioArgs, uses int) api.ScenarioArm {
	h.t.Helper()
	var a api.ScenarioArm
	resp := h.call(http.MethodPost, "/admin/scenarios", adminKey, api.ArmScenarioRequest{Fault: fault, Target: api.ScenarioTarget{Kind: kind, Value: value}, Args: args, Uses: &uses})
	if resp.status != http.StatusCreated {
		h.t.Fatalf("arm %s: %d %s", fault, resp.status, resp.body)
	}
	resp.decode(h.t, &a)
	return a
}

func (h *harness) execute() {
	h.t.Helper()
	if _, err := h.service.ExecuteDuePayouts(context.Background(), 100); err != nil {
		h.t.Fatal(err)
	}
}

func intPtr(v int) *int { return &v }

// awaitDelayStart returns once the create has consumed its delay, which it does just before it
// sleeps, so the call that follows lands inside the delay window.
func (h *harness) awaitDelayStart(delay api.ScenarioArm) {
	h.t.Helper()
	eventually(h.t, "the delayed create to take its delay", func() bool {
		return h.arms()[delay.Id].RemainingUses == 0
	})
}

func TestCloseFencesATradeReferenceForGood(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fund("corridor", "CAD", "1000000", "f-1")

	if status := h.fence("/v1/trades/by-reference/t-fenced/close"); status != api.FenceResultStatusCLOSED {
		t.Fatalf("close of an unknown reference: %s", status)
	}
	q := h.quote("q-1", "CAD", "USD", "1000")
	var late api.Trade
	resp := h.call(http.MethodPost, "/v1/trades", corridorKey, tradeRequest("t-fenced", q))
	resp.decode(t, &late)
	if resp.status != http.StatusCreated || late.Status != api.TradeStatusCLOSED || h.movements("t-fenced") != 0 {
		t.Fatalf("a create after the close: %d %s", resp.status, resp.body)
	}
	if resp := h.call(http.MethodPost, "/v1/trades", corridorKey, tradeRequest("t-fenced", q)); resp.status != http.StatusOK {
		t.Fatalf("its replay: %d", resp.status)
	}
	other := tradeRequest("t-fenced", q)
	other.SellAmount = "999"
	if resp := h.call(http.MethodPost, "/v1/trades", corridorKey, other); resp.status != http.StatusConflict {
		t.Fatalf("another body: %d", resp.status)
	}
	var read api.Trade
	h.call(http.MethodGet, "/v1/trades/by-reference/t-fenced", corridorKey, nil).decode(t, &read)
	if read.Status != api.TradeStatusCLOSED || read.TradeId != nil {
		t.Fatalf("read back %+v", read)
	}

	h.call(http.MethodPost, "/v1/trades", corridorKey, tradeRequest("t-filled", q))
	if status := h.fence("/v1/trades/by-reference/t-filled/close"); status != api.FenceResultStatusFILLED {
		t.Fatalf("close of a filled trade answered %s", status)
	}
}

func TestADelayedTradeLosesToAClose(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fund("corridor", "CAD", "1000000", "f-1")
	q := h.quote("q-late", "CAD", "USD", "1000")
	delay := h.arm(api.FaultDELAYBEFOREEXECUTION, api.ScenarioTargetKindQuote, "q-late", &api.ScenarioArgs{DelayMs: intPtr(600)})

	done := make(chan api.Trade)
	go func() {
		var tr api.Trade
		h.call(http.MethodPost, "/v1/trades", corridorKey, tradeRequest("t-late", q)).decode(t, &tr)
		done <- tr
	}()
	h.awaitDelayStart(delay)
	if status := h.fence("/v1/trades/by-reference/t-late/close"); status != api.FenceResultStatusCLOSED {
		t.Fatalf("close during the delay: %s", status)
	}
	if tr := <-done; tr.Status != api.TradeStatusCLOSED || h.movements("t-late") != 0 || h.balance("CAD") != 1000000 {
		t.Fatalf("the delayed create answered %+v with %d movements", tr, h.movements("t-late"))
	}
}

func TestCancelRacesACreateInBothOrders(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fund("corridor", "USD", "10000", "f-1")

	delay := h.arm(api.FaultDELAYBEFOREEXECUTION, api.ScenarioTargetKindPayout, "p-cancel-first", &api.ScenarioArgs{DelayMs: intPtr(600)})
	done := make(chan response)
	go func() {
		done <- h.call(http.MethodPost, "/v1/payouts", corridorKey, payoutRequest("p-cancel-first", "USD", "1000"))
	}()
	h.awaitDelayStart(delay)
	if status := h.fence("/v1/payouts/by-reference/p-cancel-first/cancel"); status != api.FenceResultStatusCANCELLED {
		t.Fatalf("cancel of an unknown reference: %s", status)
	}
	var late api.Payout
	resp := <-done
	resp.decode(t, &late)
	if resp.status != http.StatusCreated || late.Status != api.PayoutStatusCANCELLED || h.movements("p-cancel-first") != 0 {
		t.Fatalf("the delayed create: %d %s with %d movements", resp.status, resp.body, h.movements("p-cancel-first"))
	}

	h.call(http.MethodPost, "/v1/payouts", corridorKey, payoutRequest("p-create-first", "USD", "1000"))
	for range 2 {
		if status := h.fence("/v1/payouts/by-reference/p-create-first/cancel"); status != api.FenceResultStatusCANCELLED {
			t.Fatalf("cancel of an accepted payout: %s", status)
		}
	}
	h.execute()
	h.execute()
	if p := h.payout("p-create-first"); p.Status != api.PayoutStatusCANCELLED || p.Sequence != 3 || h.movements("p-create-first") != 2 || h.balance("USD") != 10000 {
		t.Fatalf("payout %+v, %d movements, USD %d; want CANCELLED, debit and refund, 10000", p, h.movements("p-create-first"), h.balance("USD"))
	}

	h.call(http.MethodPost, "/v1/payouts", corridorKey, payoutRequest("p-done", "USD", "1000"))
	h.execute()
	h.execute()
	if status := h.fence("/v1/payouts/by-reference/p-done/cancel"); status != api.FenceResultStatusCOMPLETED {
		t.Fatalf("cancel of a completed payout answered %s", status)
	}
}

func TestCancelAndExecutionRaceWithoutDoubleEffects(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fund("corridor", "BRL", "100000", "f-1")
	const payouts = 10
	for i := range payouts {
		h.call(http.MethodPost, "/v1/payouts", corridorKey, payoutRequest(fmt.Sprintf("p-%d", i), "BRL", "1000"))
	}
	h.execute()

	var wg sync.WaitGroup
	for i := range payouts {
		wg.Go(func() { h.fence(fmt.Sprintf("/v1/payouts/by-reference/p-%d/cancel", i)) })
	}
	wg.Go(h.execute)
	wg.Go(h.execute)
	wg.Wait()
	h.execute()

	completed := 0
	for i := range payouts {
		reference := fmt.Sprintf("p-%d", i)
		switch p := h.payout(reference); {
		case p.Status == api.PayoutStatusCOMPLETED && h.movements(reference) == 1:
			completed++
		case p.Status == api.PayoutStatusCANCELLED && h.movements(reference) == 2:
		default:
			t.Fatalf("%s ended %s with %d movements", reference, p.Status, h.movements(reference))
		}
	}
	if want := int64(100000 - 1000*completed); h.balance("BRL") != want {
		t.Fatalf("BRL %d after %d completions, want %d", h.balance("BRL"), completed, want)
	}
}
