package api_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"

	"github.com/xJohnNascimento/ditto/internal/provider"
)

type delivered struct {
	ID        string
	Timestamp int64
	Signature string
	Valid     bool
	Event     struct {
		EventID string `json:"event_id"`
		Type    string `json:"type"`
		Data    struct {
			Reference   string `json:"reference"`
			Status      string `json:"status"`
			Sequence    int    `json:"sequence"`
			FailureCode string `json:"failure_code"`
		} `json:"data"`
	}
}

type receiver struct {
	url      string
	mu       sync.Mutex
	failures int
	attempts map[string]int
	got      []delivered
}

func newReceiver(t *testing.T) *receiver {
	t.Helper()
	secrets, err := provider.ParseWebhookSecrets(webhookSecret)
	if err != nil {
		t.Fatal(err)
	}
	r := &receiver{attempts: make(map[string]int)}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		d := delivered{ID: req.Header.Get("webhook-id"), Signature: req.Header.Get("webhook-signature")}
		d.Timestamp, _ = strconv.ParseInt(req.Header.Get("webhook-timestamp"), 10, 64)
		d.Valid = d.Signature == provider.Sign(secrets, d.ID, d.Timestamp, body)
		_ = json.Unmarshal(body, &d.Event)

		r.mu.Lock()
		defer r.mu.Unlock()
		r.attempts[d.ID]++
		if r.failures > 0 {
			r.failures--
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		r.got = append(r.got, d)
	}))
	t.Cleanup(srv.Close)
	r.url = srv.URL
	return r
}

func (r *receiver) failNext(n int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failures = n
}

func (r *receiver) attemptsOf(id string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.attempts[id]
}

func (r *receiver) events(reference string) []delivered {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []delivered
	for _, d := range r.got {
		if d.Event.Data.Reference == reference {
			out = append(out, d)
		}
	}
	return out
}
