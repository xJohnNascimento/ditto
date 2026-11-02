package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strconv"
	"testing"

	"github.com/xJohnNascimento/ditto/internal/api"
)

func (h *harness) arms() map[string]api.ScenarioArm {
	h.t.Helper()
	var list api.ScenarioArmList
	h.call(http.MethodGet, "/admin/scenarios", adminKey, nil).decode(h.t, &list)
	byID := make(map[string]api.ScenarioArm)
	for _, a := range list.Arms {
		byID[a.Id] = a
	}
	return byID
}

func (h *harness) requireFiredOnce(arm api.ScenarioArm) {
	h.t.Helper()
	got := h.arms()[arm.Id]
	if got.RemainingUses != 0 || len(got.Consumptions) != 1 {
		h.t.Fatalf("%s fired %d times with %d uses left, want once", arm.Fault, len(got.Consumptions), got.RemainingUses)
	}
}

func (h *harness) post(path string, body any) error {
	h.t.Helper()
	encoded, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(h.t.Context(), http.MethodPost, h.srv.URL+path, bytes.NewReader(encoded))
	req.Header.Set("Authorization", "Bearer "+corridorKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.srv.Client().Do(req)
	if err == nil {
		_ = resp.Body.Close()
	}
	return err
}

func TestTradeFaultsFireOnceOnTheirQuote(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fund("corridor", "CAD", "1000000", "f-1")

	q := h.quote("q-reject", "CAD", "USD", "1000")
	reject := h.arm(api.FaultTRADEREJECT, api.ScenarioTargetKindQuote, "q-reject", nil)
	var first, second api.Trade
	h.call(http.MethodPost, "/v1/trades", corridorKey, tradeRequest("t-reject-1", q)).decode(t, &first)
	h.call(http.MethodPost, "/v1/trades", corridorKey, tradeRequest("t-reject-2", q)).decode(t, &second)
	if first.Status != api.TradeStatusREJECTED || *first.Reason != api.TradeReasonLASTLOOK || second.Status != api.TradeStatusFILLED || h.movements("t-reject-1") != 0 {
		t.Fatalf("last look: first %+v, second %+v", first, second)
	}
	h.requireFiredOnce(reject)

	q = h.quote("q-diff", "CAD", "USD", "100000")
	delta := "-5"
	difference := h.arm(api.FaultFXFILLDIFFERENCE, api.ScenarioTargetKindQuote, "q-diff", &api.ScenarioArgs{Delta: &delta})
	var filled api.Trade
	h.call(http.MethodPost, "/v1/trades", corridorKey, tradeRequest("t-diff", q)).decode(t, &filled)
	if filled.Status != api.TradeStatusFILLED || *filled.BuyAmount != "73495" || q.BuyAmount != "73500" {
		t.Fatalf("fill difference: delivered %s against a quote of %s", *filled.BuyAmount, q.BuyAmount)
	}
	h.requireFiredOnce(difference)

	h.fund("corridor", "CAD", "1000", "f-2")
	q = h.quote("q-drop", "CAD", "USD", "1000")
	drop := h.arm(api.FaultTRADECOMMITDROPREPLY, api.ScenarioTargetKindQuote, "q-drop", nil)
	if err := h.post("/v1/trades", tradeRequest("t-drop", q)); err == nil {
		t.Fatal("the reply must be lost after the commit")
	}
	var read api.Trade
	h.call(http.MethodGet, "/v1/trades/by-reference/t-drop", corridorKey, nil).decode(t, &read)
	retry := h.call(http.MethodPost, "/v1/trades", corridorKey, tradeRequest("t-drop", q))
	if read.Status != api.TradeStatusFILLED || retry.status != http.StatusOK || h.movements("t-drop") != 2 {
		t.Fatalf("after a lost reply: read %+v, retry %d, %d movements", read, retry.status, h.movements("t-drop"))
	}
	h.requireFiredOnce(drop)
}

func TestPayoutFaultsFireOnceOnTheirPayout(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fund("corridor", "USD", "10000", "f-1")

	code := "AC04"
	fail := h.arm(api.FaultPAYOUTFAIL, api.ScenarioTargetKindPayout, "p-fail", &api.ScenarioArgs{Code: &code})
	h.call(http.MethodPost, "/v1/payouts", corridorKey, payoutRequest("p-fail", "USD", "1000"))
	h.execute()
	h.execute()
	if p := h.payout("p-fail"); p.Status != api.PayoutStatusFAILED || *p.FailureCode != "AC04" || h.movements("p-fail") != 2 || h.balance("USD") != 10000 {
		t.Fatalf("failed payout %+v with %d movements and USD %d", p, h.movements("p-fail"), h.balance("USD"))
	}
	h.requireFiredOnce(fail)

	drop := h.arm(api.FaultPAYOUTCOMMITDROPREPLY, api.ScenarioTargetKindPayout, "p-drop", nil)
	if err := h.post("/v1/payouts", payoutRequest("p-drop", "USD", "1000")); err == nil {
		t.Fatal("the reply must be lost after the commit")
	}
	if p := h.payout("p-drop"); p.Status != api.PayoutStatusACCEPTED {
		t.Fatalf("after a lost reply the payout is %s", p.Status)
	}
	if retry := h.call(http.MethodPost, "/v1/payouts", corridorKey, payoutRequest("p-drop", "USD", "1000")); retry.status != http.StatusOK || h.movements("p-drop") != 1 {
		t.Fatalf("retry %d with %d movements", retry.status, h.movements("p-drop"))
	}
	h.requireFiredOnce(drop)
}

func TestRateLimitFaultThrottlesItsAccountOnce(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	limit := h.arm(api.FaultRATELIMIT, api.ScenarioTargetKindAccount, "corridor", &api.ScenarioArgs{DurationMs: intPtr(1500)})

	for range 2 {
		resp := h.call(http.MethodGet, "/v1/payouts/by-reference/x", corridorKey, nil)
		retryAfter, _ := strconv.Atoi(resp.header.Get("Retry-After"))
		if resp.status != http.StatusTooManyRequests || retryAfter < 1 || retryAfter > 2 {
			t.Fatalf("throttled: %d with Retry-After %q", resp.status, resp.header.Get("Retry-After"))
		}
	}
	if resp := h.call(http.MethodGet, "/v1/payouts/by-reference/x", otherKey, nil); resp.status != http.StatusNotFound {
		t.Fatalf("another account is throttled too: %d", resp.status)
	}
	eventually(t, "the throttle window to end", func() bool {
		return h.call(http.MethodGet, "/v1/payouts/by-reference/x", corridorKey, nil).status != http.StatusTooManyRequests
	})
	if resp := h.call(http.MethodGet, "/v1/payouts/by-reference/x", corridorKey, nil); resp.status != http.StatusNotFound {
		t.Fatalf("after the window: %d", resp.status)
	}
	h.requireFiredOnce(limit)
}

func TestArmingChecksTheFaultAgainstItsTarget(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	for name, body := range map[string]api.ArmScenarioRequest{
		"wrong target":   {Fault: api.FaultTRADEREJECT, Target: api.ScenarioTarget{Kind: api.ScenarioTargetKindPayout, Value: "p"}},
		"missing delay":  {Fault: api.FaultDELAYBEFOREEXECUTION, Target: api.ScenarioTarget{Kind: api.ScenarioTargetKindPayout, Value: "p"}},
		"missing code":   {Fault: api.FaultPAYOUTFAIL, Target: api.ScenarioTarget{Kind: api.ScenarioTargetKindPayout, Value: "p"}},
		"unknown fault":  {Fault: "MELTDOWN", Target: api.ScenarioTarget{Kind: api.ScenarioTargetKindPayout, Value: "p"}},
		"unknown target": {Fault: api.FaultTRADEREJECT, Target: api.ScenarioTarget{Kind: "galaxy", Value: "p"}},
	} {
		if resp := h.call(http.MethodPost, "/admin/scenarios", adminKey, body); resp.status != http.StatusBadRequest {
			t.Errorf("%s: %d %s", name, resp.status, resp.body)
		}
	}
}

func TestAnArmWithSeveralUsesFiresFromTheSchedulerEveryTime(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fund("corridor", "USD", "10000", "f-1")
	duplicate := h.armTimes(api.FaultWEBHOOKDUPLICATE, api.ScenarioTargetKindPayout, "p-dup", nil, 3)

	h.call(http.MethodPost, "/v1/payouts", corridorKey, payoutRequest("p-dup", "USD", "1000"))
	h.execute()
	h.execute()
	got := h.arms()[duplicate.Id]
	if p := h.payout("p-dup"); p.Status != api.PayoutStatusCOMPLETED || got.RemainingUses != 0 || len(got.Consumptions) != 3 {
		t.Fatalf("payout %s with %d uses left and %d consumptions, want COMPLETED, 0 and 3", p.Status, got.RemainingUses, len(got.Consumptions))
	}
}
