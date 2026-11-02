package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/xJohnNascimento/ditto/internal/api"
	"github.com/xJohnNascimento/ditto/internal/config"
	"github.com/xJohnNascimento/ditto/internal/pgtest"
	"github.com/xJohnNascimento/ditto/internal/provider"
	"github.com/xJohnNascimento/ditto/openapi"
)

const (
	corridorKey = "corridor-key-0123456789abcdef0123456789"
	otherKey    = "other-key-0123456789abcdef0123456789abc"
	adminKey    = "admin-key-0123456789abcdef0123456789abc"
)

func TestMain(m *testing.M) {
	os.Exit(pgtest.Main(m))
}

const webhookSecret = "whsec_MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=" //nolint:gosec // G101: a fixed secret for tests

type harness struct {
	t        *testing.T
	db       *pgxpool.Pool
	srv      *httptest.Server
	opts     provider.Options
	service  *provider.Service
	receiver *receiver
}

func defaultOptions() provider.Options {
	return provider.Options{QuoteTTL: time.Minute, RatePerSecond: 1000, Burst: 1000, WebhookWindow: time.Hour}
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	return newHarnessWith(t, defaultOptions())
}

func newHarnessWith(t *testing.T, opts provider.Options) *harness {
	t.Helper()
	db := pgtest.NewDatabase(t)
	service := provider.New(db, opts)
	hooks := newReceiver(t)
	accounts := []config.Account{
		{ID: "corridor", Key: corridorKey, WebhookURL: hooks.url, WebhookSecret: webhookSecret},
		{ID: "other", Key: otherKey},
	}
	rates, err := config.LoadRates("../../deploy/rates.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Bootstrap(context.Background(), accounts, rates); err != nil {
		t.Fatal(err)
	}
	handler, err := api.NewHandler(openapi.Spec, api.Config{
		Service: service, Accounts: accounts, AdminKey: adminKey, Profile: "ci", DatasetID: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return &harness{t: t, db: db, srv: srv, opts: opts, service: service, receiver: hooks}
}

type response struct {
	status int
	header http.Header
	body   []byte
}

func (r response) decode(t *testing.T, into any) {
	t.Helper()
	if err := json.Unmarshal(r.body, into); err != nil {
		t.Fatalf("decode %s: %v", r.body, err)
	}
}

func (r response) errorCode(t *testing.T) api.ErrorCode {
	t.Helper()
	var e api.Error
	r.decode(t, &e)
	return e.Code
}

func (h *harness) call(method, path, key string, body any) response {
	h.t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			h.t.Fatal(err)
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(h.t.Context(), method, h.srv.URL+path, reader)
	if err != nil {
		h.t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		h.t.Fatal(err)
	}
	return response{status: resp.StatusCode, header: resp.Header, body: data}
}

// eventually polls a condition that a background step or the database clock will make true, so a
// test never guesses how long that takes.
func eventually(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestHealthzNeedsNoKey(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	var body api.Health
	resp := h.call(http.MethodGet, "/healthz", "", nil)
	resp.decode(t, &body)
	if resp.status != http.StatusOK || body.Status != api.HealthStatusOk {
		t.Fatalf("got %d %s, want 200 ok", resp.status, resp.body)
	}
}

func TestEveryOperationIsRoutedBehindItsKey(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	doc, err := openapi3.NewLoader().LoadFromData(openapi.Spec)
	if err != nil {
		t.Fatal(err)
	}
	param := regexp.MustCompile(`\{[^}]+\}`)
	for path, item := range doc.Paths.Map() {
		for method, op := range item.Operations() {
			if op.OperationID == "getHealth" {
				continue
			}
			concrete := param.ReplaceAllString(path, "x")
			admin := len(path) > 6 && path[:7] == "/admin/"
			wrongKey := adminKey
			if admin {
				wrongKey = corridorKey
			}
			for _, key := range []string{"", wrongKey} {
				resp := h.call(method, concrete, key, nil)
				if resp.status != http.StatusUnauthorized || resp.errorCode(t) != api.ErrorCodeUnauthorized {
					t.Errorf("%s %s with key %q: got %d %s, want 401", method, concrete, key, resp.status, resp.body)
				}
			}
		}
	}
}

func TestUndeclaredPathsAndMethodsAnswerTheContractsError(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	unknown := h.call(http.MethodGet, "/v1/unknown", corridorKey, nil)
	if unknown.status != http.StatusNotFound || unknown.errorCode(t) != api.ErrorCodeNotFound || unknown.header.Get("Content-Type") != "application/json" {
		t.Fatalf("unknown path: %d %s", unknown.status, unknown.body)
	}
	wrongMethod := h.call(http.MethodDelete, "/v1/quotes", corridorKey, nil)
	if wrongMethod.status != http.StatusMethodNotAllowed || wrongMethod.errorCode(t) != api.ErrorCodeMethodNotAllowed || wrongMethod.header.Get("Allow") != "POST" {
		t.Fatalf("wrong method: %d %s, Allow %q", wrongMethod.status, wrongMethod.body, wrongMethod.header.Get("Allow"))
	}
}

func TestABodyOverTheLimitAnswers413(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	oversized := api.CreateQuoteRequest{ClientReference: strings.Repeat("x", 2<<20), SellCurrency: "CAD", BuyCurrency: "USD", SellAmount: "1"}
	if resp := h.call(http.MethodPost, "/v1/quotes", corridorKey, oversized); resp.status != http.StatusRequestEntityTooLarge || resp.errorCode(t) != api.ErrorCodePayloadTooLarge {
		t.Fatalf("provider body: %d %s", resp.status, resp.body)
	}
	huge := api.ArmScenarioRequest{Fault: api.FaultTRADEREJECT, Target: api.ScenarioTarget{Kind: api.ScenarioTargetKindQuote, Value: strings.Repeat("x", 2<<20)}}
	if resp := h.call(http.MethodPost, "/admin/scenarios", adminKey, huge); resp.status != http.StatusRequestEntityTooLarge || resp.errorCode(t) != api.ErrorCodePayloadTooLarge {
		t.Fatalf("admin body: %d %s", resp.status, resp.body)
	}
}

func TestNewHandlerRejectsAnInvalidContractOrAMissingHandler(t *testing.T) {
	if _, err := api.NewHandler([]byte("openapi: 3.0.3\n"), api.Config{}); err == nil {
		t.Fatal("want an error for a contract without info and paths")
	}
	extra := append([]byte(nil), openapi.Spec...)
	extra = append(extra[:bytes.Index(extra, []byte("components:"))], []byte(`  /v1/unbuilt:
    get:
      operationId: unbuilt
      responses:
        "200":
          description: never built
components:`+string(openapi.Spec[bytes.Index(openapi.Spec, []byte("components:"))+len("components:"):]))...)
	if _, err := api.NewHandler(extra, api.Config{}); err == nil {
		t.Fatal("want an error for an operation without a handler")
	}
}
