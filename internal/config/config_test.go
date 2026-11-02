package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	keyA = "key-a-0123456789abcdef0123456789abcdef"
	keyB = "key-b-0123456789abcdef0123456789abcdef"
)

func writeFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadAccounts(t *testing.T) {
	t.Parallel()
	env := map[string]string{ //nolint:gosec // G101: fixed values for tests
		"KEY_A": keyA, "KEY_B": keyB, "KEY_SHORT": "short", "KEY_A_AGAIN": keyA,
		"URL_A": "http://corridor:8081/webhooks", "SECRET_A": "whsec_MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw",
	}
	cases := []struct {
		name     string
		yaml     string
		accounts int
		wantErr  string
	}{
		{"one account", "accounts:\n  - {id: corridor, key_env: KEY_A}\n", 1, ""},
		{"two accounts with a webhook", "accounts:\n  - {id: corridor, key_env: KEY_A, webhook_url_env: URL_A, webhook_secret_env: SECRET_A}\n  - {id: other, key_env: KEY_B}\n", 2, ""},
		{"no accounts", "accounts: []\n", 0, "no accounts"},
		{"an uppercase id", "accounts:\n  - {id: Corridor, key_env: KEY_A}\n", 0, "ids must be unique"},
		{"an id that starts with a digit", "accounts:\n  - {id: 1corridor, key_env: KEY_A}\n", 0, "ids must be unique"},
		{"a repeated id", "accounts:\n  - {id: corridor, key_env: KEY_A}\n  - {id: corridor, key_env: KEY_B}\n", 0, "ids must be unique"},
		{"a short key", "accounts:\n  - {id: corridor, key_env: KEY_SHORT}\n", 0, "unique key of at least 32"},
		{"an unset key", "accounts:\n  - {id: corridor, key_env: KEY_UNSET}\n", 0, "unique key of at least 32"},
		{"a shared key", "accounts:\n  - {id: corridor, key_env: KEY_A}\n  - {id: other, key_env: KEY_A_AGAIN}\n", 0, "unique key of at least 32"},
		{"a webhook url without a secret", "accounts:\n  - {id: corridor, key_env: KEY_A, webhook_url_env: URL_A}\n", 0, "needs"},
		{"an unknown field", "accounts:\n  - {id: corridor, key_env: KEY_A, colour: red}\n", 0, "colour"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			accounts, err := LoadAccounts(writeFile(t, c.yaml), func(name string) string { return env[name] })
			if c.wantErr == "" && (err != nil || len(accounts) != c.accounts) {
				t.Fatalf("got %d accounts and error %v, want %d", len(accounts), err, c.accounts)
			}
			if c.wantErr != "" && (err == nil || !strings.Contains(err.Error(), c.wantErr)) {
				t.Fatalf("error %v, want one containing %q", err, c.wantErr)
			}
		})
	}
}

func TestLoadAccountsReadsKeysAndWebhooksFromTheEnvironment(t *testing.T) {
	t.Parallel()
	path := writeFile(t, "accounts:\n  - {id: corridor, key_env: KEY_A, webhook_url_env: URL_A, webhook_secret_env: SECRET_A}\n")
	env := map[string]string{"KEY_A": keyA, "URL_A": "http://hook", "SECRET_A": "secret"}
	accounts, err := LoadAccounts(path, func(name string) string { return env[name] })
	if err != nil {
		t.Fatal(err)
	}
	if a := accounts[0]; a.Key != keyA || a.WebhookURL != "http://hook" || a.WebhookSecret != "secret" {
		t.Fatalf("account %+v", a)
	}
}

func TestLoadRates(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		yaml    string
		rates   int
		wantErr string
	}{
		{"two pairs", "rates:\n  - {sell: CAD, buy: USD, rate_e8: \"73500000\"}\n  - {sell: USD, buy: CAD, rate_e8: \"136054422\"}\n", 2, ""},
		{"no rates", "rates: []\n", 0, ""},
		{"not a number", "rates:\n  - {sell: CAD, buy: USD, rate_e8: abc}\n", 0, "is invalid"},
		{"zero", "rates:\n  - {sell: CAD, buy: USD, rate_e8: \"0\"}\n", 0, "is invalid"},
		{"negative", "rates:\n  - {sell: CAD, buy: USD, rate_e8: \"-1\"}\n", 0, "is invalid"},
		{"beyond 64 bits", "rates:\n  - {sell: CAD, buy: USD, rate_e8: \"9223372036854775808\"}\n", 0, "is invalid"},
		{"a lowercase currency", "rates:\n  - {sell: cad, buy: USD, rate_e8: \"1\"}\n", 0, "is invalid"},
		{"a four-letter currency", "rates:\n  - {sell: CADD, buy: USD, rate_e8: \"1\"}\n", 0, "is invalid"},
		{"one currency twice", "rates:\n  - {sell: CAD, buy: CAD, rate_e8: \"1\"}\n", 0, "is invalid"},
		{"a repeated pair", "rates:\n  - {sell: CAD, buy: USD, rate_e8: \"1\"}\n  - {sell: CAD, buy: USD, rate_e8: \"2\"}\n", 0, "declared twice"},
		{"an unknown field", "rates:\n  - {sell: CAD, buy: USD, rate_e8: \"1\", spread: \"2\"}\n", 0, "spread"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			rates, err := LoadRates(writeFile(t, c.yaml))
			if c.wantErr == "" && (err != nil || len(rates) != c.rates) {
				t.Fatalf("got %d rates and error %v, want %d", len(rates), err, c.rates)
			}
			if c.wantErr != "" && (err == nil || !strings.Contains(err.Error(), c.wantErr)) {
				t.Fatalf("error %v, want one containing %q", err, c.wantErr)
			}
		})
	}
}

func TestLoadRatesParsesTheValue(t *testing.T) {
	t.Parallel()
	rates, err := LoadRates(writeFile(t, "rates:\n  - {sell: CAD, buy: USD, rate_e8: \"73500000\"}\n"))
	if err != nil || rates[0].RateE8 != 73_500_000 {
		t.Fatalf("rates %+v, error %v", rates, err)
	}
}

func TestLoadersRefuseAMissingFile(t *testing.T) {
	t.Parallel()
	missing := filepath.Join(t.TempDir(), "absent.yaml")
	if _, err := LoadAccounts(missing, func(string) string { return "" }); err == nil {
		t.Error("accounts: want an error for a missing file")
	}
	if _, err := LoadRates(missing); err == nil {
		t.Error("rates: want an error for a missing file")
	}
}
