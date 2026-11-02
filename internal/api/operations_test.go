package api_test

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/xJohnNascimento/ditto/internal/api"
	"github.com/xJohnNascimento/ditto/internal/provider"
)

func (h *harness) fund(account, currency, amount, reference string) {
	h.t.Helper()
	resp := h.call(http.MethodPost, "/admin/funding", adminKey, api.FundingRequest{Account: account, Reference: reference, Currency: currency, Amount: amount})
	if resp.status != http.StatusCreated {
		h.t.Fatalf("funding: %d %s", resp.status, resp.body)
	}
}

func (h *harness) balance(currency string) int64 {
	h.t.Helper()
	var balance int64
	err := h.db.QueryRow(context.Background(),
		"select coalesce((select balance from cash_account where provider_account_id = 'corridor' and currency = $1), 0)", currency,
	).Scan(&balance)
	if err != nil {
		h.t.Fatal(err)
	}
	return balance
}

func (h *harness) movements(reference string) int {
	h.t.Helper()
	var n int
	if err := h.db.QueryRow(context.Background(), "select count(*) from cash_movement where reference = $1", reference).Scan(&n); err != nil {
		h.t.Fatal(err)
	}
	return n
}

func (h *harness) quote(reference, sell, buy, amount string) api.Quote {
	h.t.Helper()
	var q api.Quote
	resp := h.call(http.MethodPost, "/v1/quotes", corridorKey, quoteRequest(reference, sell, buy, amount))
	if resp.status != http.StatusCreated {
		h.t.Fatalf("quote: %d %s", resp.status, resp.body)
	}
	resp.decode(h.t, &q)
	return q
}

func (h *harness) awaitExpiry(q api.Quote) {
	h.t.Helper()
	eventually(h.t, "the quote to expire", func() bool {
		var expired bool
		err := h.db.QueryRow(context.Background(), "select now() > expires_at from quote where id = $1", q.QuoteId).Scan(&expired)
		if err != nil {
			h.t.Fatal(err)
		}
		return expired
	})
}

func formatInt(v int64) string { return strconv.FormatInt(v, 10) }

func tradeRequest(reference string, q api.Quote) api.CreateTradeRequest {
	return api.CreateTradeRequest{Reference: reference, QuoteId: q.QuoteId, SellCurrency: q.SellCurrency, BuyCurrency: q.BuyCurrency, SellAmount: q.SellAmount}
}

func payoutRequest(reference, currency, amount string) api.CreatePayoutRequest {
	return api.CreatePayoutRequest{
		Reference: reference, Currency: currency, Amount: amount,
		Beneficiary: api.Beneficiary{Id: "beneficiary-1", Name: "Northwind", Account: "000123456789", BankCode: "BOFAUS3N", Country: "US"},
	}
}

func (h *harness) payout(reference string) api.Payout {
	h.t.Helper()
	var p api.Payout
	resp := h.call(http.MethodGet, "/v1/payouts/by-reference/"+reference, corridorKey, nil)
	if resp.status != http.StatusOK {
		h.t.Fatalf("payout %s: %d %s", reference, resp.status, resp.body)
	}
	resp.decode(h.t, &p)
	return p
}

func TestFundingIsIdempotentByReference(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	first := h.call(http.MethodPost, "/admin/funding", adminKey, api.FundingRequest{Account: "corridor", Reference: "f-1", Currency: "CAD", Amount: "1000000"})
	replay := h.call(http.MethodPost, "/admin/funding", adminKey, api.FundingRequest{Account: "corridor", Reference: "f-1", Currency: "CAD", Amount: "1000000"})
	var a, b api.Movement
	first.decode(t, &a)
	replay.decode(t, &b)
	if first.status != http.StatusCreated || replay.status != http.StatusOK || a != b || a.Sequence != 1 || a.BalanceAfter != "1000000" {
		t.Fatalf("first %d %+v, replay %d %+v", first.status, a, replay.status, b)
	}
	for name, c := range map[string]struct {
		body api.FundingRequest
		want int
	}{
		"changed body":      {api.FundingRequest{Account: "corridor", Reference: "f-1", Currency: "CAD", Amount: "1"}, http.StatusConflict},
		"over-withdrawal":   {api.FundingRequest{Account: "corridor", Reference: "f-2", Currency: "CAD", Amount: "-1000001"}, http.StatusBadRequest},
		"unknown account":   {api.FundingRequest{Account: "nobody", Reference: "f-3", Currency: "CAD", Amount: "1"}, http.StatusBadRequest},
		"zero":              {api.FundingRequest{Account: "corridor", Reference: "f-4", Currency: "CAD", Amount: "0"}, http.StatusBadRequest},
		"provider key used": {api.FundingRequest{Account: "corridor", Reference: "f-5", Currency: "CAD", Amount: "1"}, http.StatusUnauthorized},
	} {
		key := adminKey
		if name == "provider key used" {
			key = corridorKey
		}
		if resp := h.call(http.MethodPost, "/admin/funding", key, c.body); resp.status != c.want {
			t.Errorf("%s: got %d %s, want %d", name, resp.status, resp.body, c.want)
		}
	}
	if h.balance("CAD") != 1000000 {
		t.Fatalf("CAD balance %d, want 1000000", h.balance("CAD"))
	}
}

func TestATradeFillsOnceAgainstItsQuote(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fund("corridor", "CAD", "1000000", "f-1")
	q := h.quote("q-1", "CAD", "USD", "1000000")

	const callers = 12
	var wg sync.WaitGroup
	responses := make([]response, callers)
	for i := range callers {
		wg.Go(func() {
			responses[i] = h.call(http.MethodPost, "/v1/trades", corridorKey, tradeRequest("t-1", q))
		})
	}
	wg.Wait()
	created := 0
	var first api.Trade
	responses[0].decode(t, &first)
	for i, resp := range responses {
		var tr api.Trade
		resp.decode(t, &tr)
		if resp.status == http.StatusCreated {
			created++
		}
		if tr.Status != api.TradeStatusFILLED || *tr.TradeId != *first.TradeId || *tr.BuyAmount != "735000" {
			t.Fatalf("caller %d: %d %s", i, resp.status, resp.body)
		}
	}
	if created != 1 || h.movements("t-1") != 2 || h.balance("CAD") != 0 || h.balance("USD") != 735000 {
		t.Fatalf("%d created, %d movements, CAD %d, USD %d; want 1, 2, 0, 735000", created, h.movements("t-1"), h.balance("CAD"), h.balance("USD"))
	}

	var read api.Trade
	h.call(http.MethodGet, "/v1/trades/by-reference/t-1", corridorKey, nil).decode(t, &read)
	if *read.TradeId != *first.TradeId || read.Status != api.TradeStatusFILLED {
		t.Fatalf("read back %+v", read)
	}
	if resp := h.call(http.MethodGet, "/v1/trades/by-reference/t-unknown", corridorKey, nil); resp.status != http.StatusNotFound {
		t.Fatalf("unknown reference: %d", resp.status)
	}
	if resp := h.call(http.MethodGet, "/v1/trades/by-reference/t-1", otherKey, nil); resp.status != http.StatusNotFound {
		t.Fatalf("another account sees the trade: %d", resp.status)
	}
}

func TestTradeRejectionsAndRefusals(t *testing.T) {
	t.Parallel()
	h := newHarnessWith(t, provider.Options{QuoteTTL: 300 * time.Millisecond, RatePerSecond: 1000, Burst: 1000})
	h.fund("corridor", "CAD", "500", "f-1")

	poor := h.quote("q-poor", "CAD", "USD", "1000")
	var rejected api.Trade
	resp := h.call(http.MethodPost, "/v1/trades", corridorKey, tradeRequest("t-poor", poor))
	resp.decode(t, &rejected)
	if resp.status != http.StatusCreated || rejected.Status != api.TradeStatusREJECTED || *rejected.Reason != api.TradeReasonINSUFFICIENTBALANCE || h.movements("t-poor") != 0 {
		t.Fatalf("insufficient balance: %d %s", resp.status, resp.body)
	}

	expiring := h.quote("q-expiring", "CAD", "USD", "100")
	h.awaitExpiry(expiring)
	var expired api.Trade
	h.call(http.MethodPost, "/v1/trades", corridorKey, tradeRequest("t-expired", expiring)).decode(t, &expired)
	if expired.Status != api.TradeStatusREJECTED || *expired.Reason != api.TradeReasonQUOTEEXPIRED {
		t.Fatalf("expired quote: %+v", expired)
	}

	fresh := h.quote("q-fresh", "CAD", "USD", "100")
	wrong := tradeRequest("t-fresh", fresh)
	wrong.SellAmount = "101"
	if resp := h.call(http.MethodPost, "/v1/trades", corridorKey, wrong); resp.status != http.StatusBadRequest {
		t.Fatalf("a trade off its quote: %d %s", resp.status, resp.body)
	}
	if resp := h.call(http.MethodPost, "/v1/trades", otherKey, tradeRequest("t-fresh", fresh)); resp.status != http.StatusBadRequest {
		t.Fatalf("another account's quote: %d %s", resp.status, resp.body)
	}
	var filled api.Trade
	resp = h.call(http.MethodPost, "/v1/trades", corridorKey, tradeRequest("t-fresh", fresh))
	resp.decode(t, &filled)
	if resp.status != http.StatusCreated || filled.Status != api.TradeStatusFILLED {
		t.Fatalf("a refused request must leave the reference free: %d %s", resp.status, resp.body)
	}
}

func TestAnExpiredQuoteLeavesItsFaultsArmed(t *testing.T) {
	t.Parallel()
	h := newHarnessWith(t, provider.Options{QuoteTTL: 300 * time.Millisecond, RatePerSecond: 1000, Burst: 1000})
	h.fund("corridor", "CAD", "500", "f-1")
	q := h.quote("q-expired", "CAD", "USD", "100")
	delta := "-5"
	arms := []api.ScenarioArm{
		h.arm(api.FaultTRADEREJECT, api.ScenarioTargetKindQuote, "q-expired", nil),
		h.arm(api.FaultFXFILLDIFFERENCE, api.ScenarioTargetKindQuote, "q-expired", &api.ScenarioArgs{Delta: &delta}),
	}
	h.awaitExpiry(q)

	var expired api.Trade
	h.call(http.MethodPost, "/v1/trades", corridorKey, tradeRequest("t-expired", q)).decode(t, &expired)
	if expired.Status != api.TradeStatusREJECTED || *expired.Reason != api.TradeReasonQUOTEEXPIRED {
		t.Fatalf("expired quote: %+v", expired)
	}
	for _, arm := range arms {
		if got := h.arms()[arm.Id]; got.RemainingUses != 1 || len(got.Consumptions) != 0 {
			t.Errorf("%s: %d uses left and %d consumptions after a trade it could not change", arm.Fault, got.RemainingUses, len(got.Consumptions))
		}
	}
}

func TestAQuoteThatExpiresWhileTheTradeWaitsForALockIsNotFilled(t *testing.T) {
	t.Parallel()
	h := newHarnessWith(t, provider.Options{QuoteTTL: 2 * time.Second, RatePerSecond: 1000, Burst: 1000})
	h.fund("corridor", "CAD", "1000", "f-1")
	q := h.quote("q-waits", "CAD", "USD", "100")
	ctx := context.Background()
	tx, err := h.db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "select id from provider_account where id = 'corridor' for update"); err != nil {
		t.Fatal(err)
	}

	done := make(chan response, 1)
	go func() { done <- h.call(http.MethodPost, "/v1/trades", corridorKey, tradeRequest("t-waits", q)) }()
	eventually(t, "the trade to wait for the account lock", func() bool {
		var waiting bool
		err := h.db.QueryRow(ctx, `
			select exists(select from pg_stat_activity
			where datname = current_database() and wait_event_type = 'Lock' and query like '%insert into operation_reference%')`,
		).Scan(&waiting)
		return err == nil && waiting
	})
	h.awaitExpiry(q)
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}

	var trade api.Trade
	(<-done).decode(t, &trade)
	if trade.Status != api.TradeStatusREJECTED || *trade.Reason != api.TradeReasonQUOTEEXPIRED {
		t.Fatalf("a quote that expired during the lock wait: %+v", trade)
	}
}

func TestARejectedTradeKeepsTheQuotedAmount(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fund("corridor", "CAD", "500", "f-1")
	q := h.quote("q-poor", "CAD", "USD", "1000")
	delta := "-5"
	h.arm(api.FaultFXFILLDIFFERENCE, api.ScenarioTargetKindQuote, "q-poor", &api.ScenarioArgs{Delta: &delta})

	var rejected api.Trade
	h.call(http.MethodPost, "/v1/trades", corridorKey, tradeRequest("t-poor", q)).decode(t, &rejected)
	var stored int64
	if err := h.db.QueryRow(context.Background(), "select buy_amount from trade where reference = 't-poor'").Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if *rejected.Reason != api.TradeReasonINSUFFICIENTBALANCE || *rejected.BuyAmount != q.BuyAmount || formatInt(stored) != q.BuyAmount {
		t.Fatalf("rejected trade %+v stored %d, want the quoted %s", rejected, stored, q.BuyAmount)
	}
}

func TestPayoutLifecycle(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fund("corridor", "USD", "1000", "f-1")
	service := provider.New(h.db, h.opts)

	var accepted api.Payout
	resp := h.call(http.MethodPost, "/v1/payouts", corridorKey, payoutRequest("p-1", "USD", "600"))
	resp.decode(t, &accepted)
	if resp.status != http.StatusCreated || accepted.Status != api.PayoutStatusACCEPTED || accepted.Sequence != 1 || h.balance("USD") != 400 {
		t.Fatalf("accept: %d %s, USD %d", resp.status, resp.body, h.balance("USD"))
	}
	for _, want := range []struct {
		status   api.PayoutStatus
		sequence int
	}{{api.PayoutStatusPROCESSING, 2}, {api.PayoutStatusCOMPLETED, 3}} {
		if _, err := service.ExecuteDuePayouts(context.Background(), 10); err != nil {
			t.Fatal(err)
		}
		if p := h.payout("p-1"); p.Status != want.status || p.Sequence != want.sequence || *p.Amount != "600" || *p.Currency != "USD" {
			t.Fatalf("payout %+v, want %s at sequence %d", p, want.status, want.sequence)
		}
	}
	var replay api.Payout
	resp = h.call(http.MethodPost, "/v1/payouts", corridorKey, payoutRequest("p-1", "USD", "600"))
	resp.decode(t, &replay)
	if resp.status != http.StatusOK || replay.Status != api.PayoutStatusCOMPLETED || *replay.PayoutId != *accepted.PayoutId {
		t.Fatalf("replay: %d %s", resp.status, resp.body)
	}
	if resp := h.call(http.MethodPost, "/v1/payouts", corridorKey, payoutRequest("p-1", "USD", "601")); resp.status != http.StatusConflict {
		t.Fatalf("changed body: %d", resp.status)
	}

	var rejected api.Payout
	h.call(http.MethodPost, "/v1/payouts", corridorKey, payoutRequest("p-2", "USD", "401")).decode(t, &rejected)
	if rejected.Status != api.PayoutStatusREJECTED || *rejected.FailureCode != "AM04" || h.movements("p-2") != 0 || h.balance("USD") != 400 {
		t.Fatalf("over-balance payout: %+v", rejected)
	}
}

func TestOnePayoutFailingDoesNotBlockTheOthers(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fund("corridor", "USD", "10000", "f-1")
	h.call(http.MethodPost, "/v1/payouts", corridorKey, payoutRequest("p-bad", "USD", "1000"))
	h.call(http.MethodPost, "/v1/payouts", corridorKey, payoutRequest("p-good", "USD", "1000"))
	h.execute()
	// The API refuses this code, so the arm goes in directly: its payout then violates the failure_code check.
	_, err := h.db.Exec(context.Background(), `
		insert into scenario_arm (id, fault, target_kind, target_value, args, remaining_uses)
		values (gen_random_uuid(), 'PAYOUT_FAIL', 'payout', 'p-bad', '{"code": "bad"}', 1)`)
	if err != nil {
		t.Fatal(err)
	}

	advanced, err := h.service.ExecuteDuePayouts(context.Background(), 100)
	if err == nil || advanced != 1 {
		t.Fatalf("advanced %d with error %v, want one payout moved and the other's error returned", advanced, err)
	}
	if p := h.payout("p-good"); p.Status != api.PayoutStatusCOMPLETED {
		t.Fatalf("the healthy payout is %s", p.Status)
	}
	if p := h.payout("p-bad"); p.Status != api.PayoutStatusPROCESSING {
		t.Fatalf("the failing payout is %s, want it untouched", p.Status)
	}

	advanced, err = h.service.ExecuteDuePayouts(context.Background(), 100)
	if err != nil || advanced != 0 {
		t.Fatalf("second pass advanced %d with error %v, want the failed payout to be cooling down", advanced, err)
	}
}

func TestOneEffectSurvivesALostResponseAndARestart(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fund("corridor", "EUR", "10000", "f-1")

	const callers = 10
	var wg sync.WaitGroup
	ids := make([]string, callers)
	for i := range callers {
		wg.Go(func() {
			var p api.Payout
			h.call(http.MethodPost, "/v1/payouts", corridorKey, payoutRequest("p-lost", "EUR", "2500")).decode(t, &p)
			ids[i] = *p.PayoutId
		})
	}
	wg.Wait()
	for i := range callers {
		if ids[i] != ids[0] {
			t.Fatalf("retries answered payouts %v", ids)
		}
	}

	before, after := provider.New(h.db, h.opts), provider.New(h.db, h.opts)
	for range 3 {
		var wg sync.WaitGroup
		for _, instance := range []*provider.Service{before, after} {
			wg.Go(func() {
				if _, err := instance.ExecuteDuePayouts(context.Background(), 10); err != nil {
					t.Error(err)
				}
			})
		}
		wg.Wait()
	}
	var versions int
	if err := h.db.QueryRow(context.Background(), "select version from operation_reference where reference = 'p-lost'").Scan(&versions); err != nil {
		t.Fatal(err)
	}
	if p := h.payout("p-lost"); p.Status != api.PayoutStatusCOMPLETED || versions != 4 || h.movements("p-lost") != 1 || h.balance("EUR") != 7500 {
		t.Fatalf("payout %+v after %d versions, %d movements, EUR %d; want COMPLETED, 4, 1, 7500", p, versions, h.movements("p-lost"), h.balance("EUR"))
	}
}

func TestCashMovementsRollForwardPerCurrency(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fund("corridor", "CAD", "5000", "f-1")
	for i := range 4 {
		q := h.quote(fmt.Sprintf("q-%d", i), "CAD", "USD", "1000")
		h.call(http.MethodPost, "/v1/trades", corridorKey, tradeRequest(fmt.Sprintf("t-%d", i), q))
	}
	rows, err := h.db.Query(context.Background(), "select currency, sequence, amount, balance_after from cash_movement order by currency, sequence")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	expected := map[string]struct{ sequence, balance int64 }{}
	for rows.Next() {
		var (
			currency                       string
			sequence, amount, balanceAfter int64
		)
		if err := rows.Scan(&currency, &sequence, &amount, &balanceAfter); err != nil {
			t.Fatal(err)
		}
		state := expected[currency]
		state.sequence++
		state.balance += amount
		if sequence != state.sequence || balanceAfter != state.balance {
			t.Fatalf("%s movement %d: balance after %d, want sequence %d and balance %d", currency, sequence, balanceAfter, state.sequence, state.balance)
		}
		expected[currency] = state
	}
	if expected["CAD"].balance != h.balance("CAD") || expected["USD"].balance != h.balance("USD") {
		t.Fatalf("movements roll to %+v, balances are CAD %d and USD %d", expected, h.balance("CAD"), h.balance("USD"))
	}
}
