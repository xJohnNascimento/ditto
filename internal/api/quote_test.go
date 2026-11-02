package api_test

import (
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/xJohnNascimento/ditto/internal/api"
	"github.com/xJohnNascimento/ditto/internal/provider"
)

func quoteRequest(reference, sell, buy, amount string) api.CreateQuoteRequest {
	return api.CreateQuoteRequest{ClientReference: reference, SellCurrency: sell, BuyCurrency: buy, SellAmount: amount}
}

func TestQuoteFloorsTheConvertedAmount(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	cases := []struct {
		sell, buy, amount, rate, bought string
	}{
		{"CAD", "USD", "1000000", "73500000", "735000"},
		{"CAD", "USD", "999", "73500000", "734"},
		{"USD", "BRL", "1", "555555556", "5"},
	}
	for i, c := range cases {
		resp := h.call(http.MethodPost, "/v1/quotes", corridorKey, quoteRequest(fmt.Sprintf("floor-%d", i), c.sell, c.buy, c.amount))
		var q api.Quote
		resp.decode(t, &q)
		if resp.status != http.StatusCreated || q.RateE8 != c.rate || q.BuyAmount != c.bought {
			t.Errorf("%s %s→%s: got %d rate %s buy %s, want rate %s buy %s", c.amount, c.sell, c.buy, resp.status, q.RateE8, q.BuyAmount, c.rate, c.bought)
		}
		if ttl := time.Until(q.ExpiresAt); ttl < 50*time.Second || ttl > 70*time.Second {
			t.Errorf("quote expires in %s, want about a minute", ttl)
		}
	}
}

func TestQuotesAreIdempotentByClientReferenceUnderConcurrency(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	const callers = 16
	var (
		wg        sync.WaitGroup
		start     = make(chan struct{})
		statuses  = make([]int, callers)
		quoteIDs  = make([]string, callers)
		responses = make([]response, callers)
	)
	for i := range callers {
		wg.Go(func() {
			<-start
			responses[i] = h.call(http.MethodPost, "/v1/quotes", corridorKey, quoteRequest("q-1", "CAD", "USD", "1000000"))
		})
	}
	close(start)
	wg.Wait()

	created := 0
	for i, resp := range responses {
		var q api.Quote
		resp.decode(t, &q)
		statuses[i], quoteIDs[i] = resp.status, q.QuoteId
		if resp.status == http.StatusCreated {
			created++
		}
		if q.QuoteId != quoteIDs[0] {
			t.Fatalf("caller %d got quote %s, caller 0 got %s", i, q.QuoteId, quoteIDs[0])
		}
	}
	if created != 1 {
		t.Fatalf("statuses %v, want exactly one 201", statuses)
	}

	conflict := h.call(http.MethodPost, "/v1/quotes", corridorKey, quoteRequest("q-1", "CAD", "USD", "1000001"))
	if conflict.status != http.StatusConflict || conflict.errorCode(t) != api.ErrorCodeReferenceConflict {
		t.Fatalf("changed body: %d %s, want 409", conflict.status, conflict.body)
	}
	other := h.call(http.MethodPost, "/v1/quotes", otherKey, quoteRequest("q-1", "CAD", "USD", "100"))
	if other.status != http.StatusCreated {
		t.Fatalf("another account's reference space: %d %s", other.status, other.body)
	}
}

func TestRequestsAreValidatedAgainstTheContract(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	for name, body := range map[string]any{
		"lowercase currency":  quoteRequest("v", "cad", "USD", "1"),
		"leading zero":        quoteRequest("v", "CAD", "USD", "01"),
		"fraction":            quoteRequest("v", "CAD", "USD", "1.5"),
		"empty reference":     quoteRequest("", "CAD", "USD", "1"),
		"missing field":       map[string]string{"client_reference": "v", "sell_currency": "CAD", "buy_currency": "USD"},
		"same currency":       quoteRequest("v", "CAD", "CAD", "1"),
		"pair not offered":    quoteRequest("v", "CAD", "JPY", "1"),
		"converts to nothing": quoteRequest("v", "BRL", "EUR", "1"),
		"over 64 bits":        quoteRequest("v", "CAD", "USD", "92233720368547758080"),
	} {
		resp := h.call("POST", "/v1/quotes", corridorKey, body)
		if resp.status != http.StatusBadRequest || resp.errorCode(t) != api.ErrorCodeValidationFailed {
			t.Errorf("%s: got %d %s, want 400 validation_failed", name, resp.status, resp.body)
		}
	}
}

func TestARateChangeAffectsOnlyNewQuotes(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	before := h.call(http.MethodPost, "/v1/quotes", corridorKey, quoteRequest("rate-1", "CAD", "USD", "1000000"))
	set := h.call(http.MethodPost, "/admin/rates", adminKey, api.Rate{SellCurrency: "CAD", BuyCurrency: "USD", RateE8: "74000000"})
	if set.status != http.StatusOK {
		t.Fatalf("set rate: %d %s", set.status, set.body)
	}
	replay := h.call(http.MethodPost, "/v1/quotes", corridorKey, quoteRequest("rate-1", "CAD", "USD", "1000000"))
	after := h.call(http.MethodPost, "/v1/quotes", corridorKey, quoteRequest("rate-2", "CAD", "USD", "1000000"))
	var first, again, second api.Quote
	before.decode(t, &first)
	replay.decode(t, &again)
	after.decode(t, &second)
	if replay.status != http.StatusOK || again != first || second.BuyAmount != "740000" {
		t.Fatalf("replay %d %+v (want %+v), new quote buys %s, want 740000", replay.status, again, first, second.BuyAmount)
	}
}

func TestTheBucketRefusesPastItsBurstWithRetryAfter(t *testing.T) {
	t.Parallel()
	h := newHarnessWith(t, provider.Options{QuoteTTL: time.Minute, RatePerSecond: 0.5, Burst: 5})
	for i := range 5 {
		if resp := h.call(http.MethodPost, "/v1/quotes", corridorKey, quoteRequest(fmt.Sprintf("b-%d", i), "CAD", "USD", "100")); resp.status != http.StatusCreated {
			t.Fatalf("request %d within the burst: %d %s", i, resp.status, resp.body)
		}
	}
	refused := h.call(http.MethodPost, "/v1/quotes", corridorKey, quoteRequest("b-refused", "CAD", "USD", "100"))
	if refused.status != http.StatusTooManyRequests || refused.errorCode(t) != api.ErrorCodeRateLimited {
		t.Fatalf("over the burst: %d %s, want 429", refused.status, refused.body)
	}
	retryAfter, err := strconv.Atoi(refused.header.Get("Retry-After"))
	if err != nil || retryAfter < 1 || retryAfter > 2 {
		t.Fatalf("Retry-After %q, want 1 or 2 seconds at half a token per second", refused.header.Get("Retry-After"))
	}
	if resp := h.call(http.MethodPost, "/v1/quotes", otherKey, quoteRequest("b-other", "CAD", "USD", "100")); resp.status != http.StatusCreated {
		t.Fatalf("another account has its own bucket: %d", resp.status)
	}
	if resp := h.call(http.MethodPost, "/admin/rates", adminKey, api.Rate{SellCurrency: "CAD", BuyCurrency: "USD", RateE8: "73500000"}); resp.status != http.StatusOK {
		t.Fatalf("admin calls do not spend the budget: %d", resp.status)
	}

	var quotes, throttled int
	err = h.db.QueryRow(t.Context(), `
		select (select count(*) from quote where client_reference = 'b-refused'),
		       (select count(*) from request_observation where throttled and route = 'createQuote')`,
	).Scan(&quotes, &throttled)
	if err != nil || quotes != 0 || throttled != 1 {
		t.Fatalf("refused quote created %d times, %d throttled observations (%v); want 0 and 1", quotes, throttled, err)
	}
}
