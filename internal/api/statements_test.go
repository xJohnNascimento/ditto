package api_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/xJohnNascimento/ditto/internal/api"
)

type capturedStatement struct {
	manifest api.Statement
	raw      []json.RawMessage
	lines    []api.StatementLine
}

func (h *harness) statement(after string) capturedStatement {
	h.t.Helper()
	request := api.CreateStatementRequest{}
	if after != "" {
		request.AfterStatementId = &after
	}
	var st capturedStatement
	resp := h.call(http.MethodPost, "/v1/statements", corridorKey, request)
	if resp.status != http.StatusCreated {
		h.t.Fatalf("statement: %d %s", resp.status, resp.body)
	}
	resp.decode(h.t, &st.manifest)
	cursor := ""
	for {
		var page struct {
			Lines      []json.RawMessage `json:"lines"`
			NextCursor string            `json:"next_cursor"`
		}
		h.call(http.MethodGet, "/v1/statements/"+st.manifest.StatementId+"/pages?cursor="+cursor, corridorKey, nil).decode(h.t, &page)
		st.raw = append(st.raw, page.Lines...)
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	for _, raw := range st.raw {
		var l api.StatementLine
		if err := json.Unmarshal(raw, &l); err != nil {
			h.t.Fatal(err)
		}
		st.lines = append(st.lines, l)
	}
	return st
}

func requireRollsForward(t *testing.T, st capturedStatement) map[string]bool {
	t.Helper()
	hash := sha256.New()
	for _, raw := range st.raw {
		hash.Write(raw)
	}
	if hex.EncodeToString(hash.Sum(nil)) != st.manifest.ContentHash || int64(len(st.raw)) != st.manifest.LineCount {
		t.Fatalf("pages do not match the manifest's hash and %d lines", st.manifest.LineCount)
	}
	running := map[string]int64{}
	next := map[string]int64{}
	for _, c := range st.manifest.Currencies {
		opening, _ := strconv.ParseInt(c.OpeningBalance, 10, 64)
		running[c.Currency], next[c.Currency] = opening, c.FromSequence
	}
	outcomes := map[string]bool{}
	for i, l := range st.lines {
		if l.LineId != strconv.Itoa(i+1) {
			t.Fatalf("line %d has id %s", i+1, l.LineId)
		}
		switch l.Kind {
		case api.StatementLineKindMOVEMENT:
			m := l.Movement
			amount, _ := strconv.ParseInt(m.Amount, 10, 64)
			running[m.Currency] += amount
			if m.Sequence != next[m.Currency] || strconv.FormatInt(running[m.Currency], 10) != m.BalanceAfter {
				t.Fatalf("%s movement %d: balance after %s, rolled %d, expected sequence %d", m.Currency, m.Sequence, m.BalanceAfter, running[m.Currency], next[m.Currency])
			}
			next[m.Currency]++
		case api.StatementLineKindOUTCOME:
			key := string(l.Outcome.Operation) + "/" + l.Outcome.Reference
			if outcomes[key] {
				t.Fatalf("outcome %s appears twice", key)
			}
			outcomes[key] = true
		case api.StatementLineKindBALANCE:
		}
	}
	for _, c := range st.manifest.Currencies {
		if strconv.FormatInt(running[c.Currency], 10) != c.ClosingBalance || next[c.Currency] != c.ToSequence+1 {
			t.Fatalf("%s rolls to %d through sequence %d, the manifest closes at %s through %d", c.Currency, running[c.Currency], next[c.Currency]-1, c.ClosingBalance, c.ToSequence)
		}
	}
	return outcomes
}

func TestStatementsRollForwardExactlyAndChain(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	for i := range 120 {
		h.fund("corridor", "CAD", "1000", fmt.Sprintf("f-%d", i))
	}
	h.fund("corridor", "USD", "5000", "f-usd")
	q := h.quote("q-1", "CAD", "USD", "10000")
	h.call(http.MethodPost, "/v1/trades", corridorKey, tradeRequest("t-1", q))
	h.call(http.MethodPost, "/v1/payouts", corridorKey, payoutRequest("p-done", "USD", "1000"))
	h.call(http.MethodPost, "/v1/payouts", corridorKey, payoutRequest("p-cancelled", "USD", "1000"))
	h.fence("/v1/payouts/by-reference/p-cancelled/cancel")
	h.fence("/v1/trades/by-reference/t-fenced/close")
	h.execute()
	h.execute()

	first := h.statement("")
	outcomes := requireRollsForward(t, first)
	for _, want := range []string{"TRADE/t-1", "PAYOUT/p-done", "PAYOUT/p-cancelled", "TRADE/t-fenced"} {
		if !outcomes[want] {
			t.Fatalf("first statement misses outcome %s: %v", want, outcomes)
		}
	}
	if len(first.raw) <= 100 {
		t.Fatalf("%d lines do not exercise paging", len(first.raw))
	}

	h.call(http.MethodPost, "/v1/payouts", corridorKey, payoutRequest("p-later", "USD", "500"))
	h.execute()
	h.execute()
	h.fund("corridor", "CAD", "-500", "f-withdraw")
	second := h.statement(first.manifest.StatementId)
	later := requireRollsForward(t, second)
	if len(later) != 1 || !later["PAYOUT/p-later"] {
		t.Fatalf("the chained statement reports outcomes %v, want only p-later", later)
	}
	for i, c := range second.manifest.Currencies {
		if c.OpeningBalance != first.manifest.Currencies[i].ClosingBalance || c.FromSequence != first.manifest.Currencies[i].ToSequence+1 {
			t.Fatalf("%s opens at %s from %d; the previous closed at %s through %d", c.Currency, c.OpeningBalance, c.FromSequence,
				first.manifest.Currencies[i].ClosingBalance, first.manifest.Currencies[i].ToSequence)
		}
	}

	again := h.statement("")
	if len(again.raw) != len(first.raw)+len(second.raw)-len(second.manifest.Currencies) {
		t.Fatalf("a full statement has %d lines, want the two chained ones without the second's balance lines", len(again.raw))
	}
	var line api.StatementLine
	h.call(http.MethodGet, "/v1/statements/"+first.manifest.StatementId+"/lines/1", corridorKey, nil).decode(t, &line)
	if line.Kind != api.StatementLineKindBALANCE || line.LineId != "1" {
		t.Fatalf("line 1: %+v", line)
	}
	h.fund("corridor", "CAD", "1", "f-after")
	stillFrozen := h.call(http.MethodGet, "/v1/statements/"+first.manifest.StatementId+"/lines/"+strconv.Itoa(len(first.raw)), corridorKey, nil)
	if string(stillFrozen.body[:len(stillFrozen.body)-1]) != string(first.raw[len(first.raw)-1]) {
		t.Fatal("a statement line changed after later activity")
	}
	if resp := h.call(http.MethodGet, "/v1/statements/"+first.manifest.StatementId+"/pages", otherKey, nil); resp.status != http.StatusNotFound {
		t.Fatalf("another account reads the statement: %d", resp.status)
	}
}

func TestStatusAPILagHidesACompletionFromReadsOnly(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fund("corridor", "USD", "5000", "f-1")
	h.arm(api.FaultSTATUSAPILAG, api.ScenarioTargetKindPayout, "p-lag", &api.ScenarioArgs{DurationMs: intPtr(800)})
	h.call(http.MethodPost, "/v1/payouts", corridorKey, payoutRequest("p-lag", "USD", "1000"))
	h.execute()
	h.execute()
	if _, err := h.service.DeliverWebhooks(t.Context(), h.srv.Client(), 100); err != nil {
		t.Fatal(err)
	}

	if p := h.payout("p-lag"); p.Status != api.PayoutStatusPROCESSING || p.Sequence != 2 {
		t.Fatalf("a lagged read: %+v", p)
	}
	st := h.statement("")
	completed := false
	for _, l := range st.lines {
		if l.Outcome != nil && l.Outcome.Reference == "p-lag" && l.Outcome.Status == api.OutcomeStatusCOMPLETED {
			completed = true
		}
	}
	if got := statuses(h.receiver.events("p-lag")); !completed || len(got) != 2 {
		t.Fatalf("statement shows completion %v; webhooks delivered %v", completed, got)
	}

	eventually(t, "the lag to end", func() bool { return h.payout("p-lag").Status == api.PayoutStatusCOMPLETED })
	h.drain()
	if p := h.payout("p-lag"); p.Status != api.PayoutStatusCOMPLETED {
		t.Fatalf("after the lag: %+v", p)
	}
	if got := statuses(h.receiver.events("p-lag")); len(got) != 3 || got[2] != "COMPLETED" {
		t.Fatalf("after the lag, webhooks %v", got)
	}
}

func TestAStuckPayoutWaitsForACancel(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fund("corridor", "USD", "5000", "f-1")
	h.arm(api.FaultPAYOUTSTUCKCANCELLABLE, api.ScenarioTargetKindPayout, "p-stuck", nil)
	h.call(http.MethodPost, "/v1/payouts", corridorKey, payoutRequest("p-stuck", "USD", "1000"))
	for range 4 {
		h.execute()
	}
	if p := h.payout("p-stuck"); p.Status != api.PayoutStatusPROCESSING {
		t.Fatalf("stuck payout is %s", p.Status)
	}
	if status := h.fence("/v1/payouts/by-reference/p-stuck/cancel"); status != api.FenceResultStatusCANCELLED || h.balance("USD") != 5000 {
		t.Fatalf("cancel of a stuck payout: %s, USD %d", status, h.balance("USD"))
	}
}

func TestAContradictoryFinalHintNeverChangesTheFact(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fund("corridor", "USD", "5000", "f-1")
	h.arm(api.FaultCONTRADICTORYFINALHINT, api.ScenarioTargetKindPayout, "p-hint", nil)
	h.call(http.MethodPost, "/v1/payouts", corridorKey, payoutRequest("p-hint", "USD", "1000"))
	h.execute()
	h.execute()
	h.drain()
	if got := statuses(h.receiver.events("p-hint")); len(got) != 4 || got[2] != "COMPLETED" || got[3] != "FAILED" {
		t.Fatalf("delivered %v, want a contradicting FAILED after COMPLETED", got)
	}
	if p := h.payout("p-hint"); p.Status != api.PayoutStatusCOMPLETED {
		t.Fatalf("the fact changed to %s", p.Status)
	}
}

func TestRequestStatsCountAdmittedAndThrottledRequests(t *testing.T) {
	t.Parallel()
	opts := defaultOptions()
	opts.RatePerSecond, opts.Burst = 0.1, 3
	h := newHarnessWith(t, opts)
	since := time.Now().Add(-time.Second).UTC().Format(time.RFC3339Nano)
	// The peak counts admitted requests per wall-clock second, so the burst starts at the top of
	// one: three requests that straddle a second would count as two.
	time.Sleep(time.Until(time.Now().Truncate(time.Second).Add(time.Second)))
	for range 5 {
		h.call(http.MethodGet, "/v1/payouts/by-reference/none", corridorKey, nil)
	}
	var stats api.RequestStats
	h.call(http.MethodGet, "/admin/requests?since="+since, adminKey, nil).decode(t, &stats)
	if len(stats.Routes) != 1 || stats.Routes[0].Route != "getPayoutByReference" || stats.Routes[0].Count != 5 || stats.Routes[0].Throttled != 2 || stats.PeakPerSecond != 3 {
		t.Fatalf("stats %+v", stats)
	}
}

func TestResetNeedsItsDatasetAndRestoresTheConfiguration(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fund("corridor", "CAD", "1000", "f-1")
	h.call(http.MethodPost, "/admin/rates", adminKey, api.Rate{SellCurrency: "CAD", BuyCurrency: "USD", RateE8: "1"})

	if resp := h.call(http.MethodPost, "/admin/reset", adminKey, api.ResetRequest{DatasetId: "production"}); resp.status != http.StatusForbidden || resp.errorCode(t) != api.ErrorCodeForbidden {
		t.Fatalf("reset of another dataset: %d %s", resp.status, resp.body)
	}
	if resp := h.call(http.MethodPost, "/admin/reset", adminKey, api.ResetRequest{DatasetId: "test"}); resp.status != http.StatusNoContent {
		t.Fatalf("reset: %d %s", resp.status, resp.body)
	}
	if h.balance("CAD") != 0 {
		t.Fatalf("CAD %d after reset", h.balance("CAD"))
	}
	if q := h.quote("q-after", "CAD", "USD", "1000000"); q.RateE8 != "73500000" {
		t.Fatalf("rate after reset %s, want the configured 73500000", q.RateE8)
	}
	h.fund("corridor", "CAD", "1000", "f-1")
}
