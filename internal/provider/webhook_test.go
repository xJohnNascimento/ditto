package provider

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestSignMatchesTheStandardWebhooksVector(t *testing.T) {
	t.Parallel()
	secrets, err := ParseWebhookSecrets("whsec_MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw")
	if err != nil {
		t.Fatal(err)
	}
	got := Sign(secrets, "msg_p5jXN8AQM9LWM0D4loKWxJek", 1614265330, []byte(`{"test": 2432232314}`))
	if want := "v1,g0hM9SsE+OTPJTGt/tmIKtSyZlE3uFJELVlNIOLJ1OE="; got != want {
		t.Fatalf("signature %s, want %s", got, want)
	}
}

func TestSignWithTwoSecretsAnswersOneSignaturePerSecret(t *testing.T) {
	t.Parallel()
	secrets, err := ParseWebhookSecrets("whsec_MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw whsec_MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=")
	if err != nil {
		t.Fatal(err)
	}
	signatures := strings.Fields(Sign(secrets, "msg_1", 1, []byte("{}")))
	if len(signatures) != 2 || signatures[0] == signatures[1] {
		t.Fatalf("signatures %v, want two different ones", signatures)
	}
}

func TestParseWebhookSecrets(t *testing.T) {
	t.Parallel()
	valid := "whsec_MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw"
	cases := []struct {
		name    string
		value   string
		keys    int
		wantErr bool
	}{
		{"one secret", valid, 1, false},
		{"a rotation", valid + " " + valid, 2, false},
		{"extra whitespace", "  " + valid + "\t\n", 1, false},
		{"empty", "", 0, true},
		{"blank", "   ", 0, true},
		{"no prefix", "MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw", 0, true},
		{"not base64", "whsec_not base64!", 0, true},
		{"22 bytes", "whsec_MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NQ==", 0, true},
		{"one bad secret in a rotation", valid + " whsec_short", 0, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			keys, err := ParseWebhookSecrets(c.value)
			if (err != nil) != c.wantErr || len(keys) != c.keys {
				t.Fatalf("got %d keys and error %v, want %d keys and error %v", len(keys), err, c.keys, c.wantErr)
			}
		})
	}
}

func TestRetryBackoff(t *testing.T) {
	t.Parallel()
	const base = 2 * time.Second
	cases := []struct {
		name     string
		base     time.Duration
		attempts int
		want     time.Duration
	}{
		{"first attempt", base, 1, 2 * time.Second},
		{"second attempt", base, 2, 4 * time.Second},
		{"last below the cap", base, 11, 2048 * time.Second},
		{"first past the cap", base, 12, maxBackoff},
		{"where a plain shift overflows", base, 34, maxBackoff},
		{"far beyond it", base, 1000, maxBackoff},
		{"no attempts yet", base, 0, 2 * time.Second},
		{"no base", 0, 5, 0},
		{"a base above the cap", 2 * maxBackoff, 1, maxBackoff},
		{"a one nanosecond base", time.Nanosecond, 1000, maxBackoff},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := retryBackoff(c.base, c.attempts); got != c.want {
				t.Fatalf("retryBackoff(%s, %d) = %s, want %s", c.base, c.attempts, got, c.want)
			}
		})
	}
}

func TestLeaseCoversTheWholeBatch(t *testing.T) {
	t.Parallel()
	if got, want := leaseFor(&http.Client{Timeout: 5 * time.Second}, 50), 50*5*time.Second+leaseMargin; got != want {
		t.Fatalf("lease %s, want %s", got, want)
	}
	if got, want := leaseFor(&http.Client{}, 10), 10*defaultSendTimeout+leaseMargin; got != want {
		t.Fatalf("lease without a client timeout %s, want %s", got, want)
	}
}

func TestPayoutHints(t *testing.T) {
	t.Parallel()
	cases := []struct {
		status PayoutStatus
		want   string
		hinted bool
	}{
		{PayoutStatusAccepted, "", false},
		{PayoutStatusRejected, "", false},
		{PayoutStatusProcessing, FalseFailedHint, true},
		{PayoutStatusCompleted, ContradictoryFinalHint, true},
		{PayoutStatusFailed, ContradictoryFinalHint, true},
		{PayoutStatusCancelled, ContradictoryFinalHint, true},
	}
	for _, c := range cases {
		t.Run(string(c.status), func(t *testing.T) {
			t.Parallel()
			got, ok := payoutHints[c.status]
			if ok != c.hinted || got != c.want {
				t.Fatalf("payoutHints[%s] = %q, %t; want %q, %t", c.status, got, ok, c.want, c.hinted)
			}
		})
	}
}

func TestContradictingEvent(t *testing.T) {
	t.Parallel()
	id := uuid.MustParse("0190a5c0-0000-7000-8000-000000000001")
	payout := func(status PayoutStatus, sequence int, failureCode string) Payout {
		return Payout{Reference: "p-1", Status: status, Sequence: sequence, HasTerms: true, ID: id, Currency: "USD", Amount: 600, FailureCode: failureCode}
	}
	cases := []struct {
		name            string
		payout          Payout
		hint            *Arm
		wantStatus      PayoutStatus
		wantFailureCode string
	}{
		{"processing is announced as failed with the armed code", payout(PayoutStatusProcessing, 2, ""), &Arm{Args: FaultArgs{Code: "AM04"}}, PayoutStatusFailed, "AM04"},
		{"processing without a code falls back to AC04", payout(PayoutStatusProcessing, 2, ""), &Arm{}, PayoutStatusFailed, "AC04"},
		{"completed is announced as failed", payout(PayoutStatusCompleted, 3, ""), &Arm{Args: FaultArgs{Code: "AC04"}}, PayoutStatusFailed, "AC04"},
		{"failed is announced as completed and drops the code", payout(PayoutStatusFailed, 3, "AM04"), &Arm{Args: FaultArgs{Code: "AC04"}}, PayoutStatusCompleted, ""},
		{"cancelled is announced as completed", payout(PayoutStatusCancelled, 3, ""), &Arm{}, PayoutStatusCompleted, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := contradictingEvent(c.payout, c.hint)
			if got.Status != c.wantStatus || got.FailureCode != c.wantFailureCode || got.Sequence != finalSequence {
				t.Fatalf("event %+v, want status %s, failure code %q, sequence %d", got, c.wantStatus, c.wantFailureCode, finalSequence)
			}
			if got.Reference != "p-1" || got.PayoutID != id.String() || got.Currency != "USD" || got.Amount != "600" {
				t.Fatalf("event %+v does not carry the payout's identity", got)
			}
		})
	}
}
