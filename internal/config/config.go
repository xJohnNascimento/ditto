package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"

	"go.yaml.in/yaml/v3"
)

const minKeyLength = 32

var (
	accountIDPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	currencyPattern  = regexp.MustCompile(`^[A-Z]{3}$`)
)

type Account struct {
	ID               string `yaml:"id"`
	KeyEnv           string `yaml:"key_env"`
	WebhookURLEnv    string `yaml:"webhook_url_env"`
	WebhookSecretEnv string `yaml:"webhook_secret_env"`
	Key              string `yaml:"-"`
	WebhookURL       string `yaml:"-"`
	WebhookSecret    string `yaml:"-"`
}

type Rate struct {
	Sell   string `yaml:"sell"`
	Buy    string `yaml:"buy"`
	RateE8 int64  `yaml:"-"`
	Raw    string `yaml:"rate_e8"`
}

func LoadAccounts(path string, getenv func(string) string) ([]Account, error) {
	var file struct {
		Accounts []Account `yaml:"accounts"`
	}
	if err := decode(path, &file); err != nil {
		return nil, err
	}
	if len(file.Accounts) == 0 {
		return nil, errors.New("accounts file: no accounts")
	}
	ids := make(map[string]bool)
	keys := make(map[string]bool)
	for i := range file.Accounts {
		a := &file.Accounts[i]
		if !accountIDPattern.MatchString(a.ID) || ids[a.ID] {
			return nil, fmt.Errorf("account %q: ids must be unique and match %s", a.ID, accountIDPattern)
		}
		ids[a.ID] = true
		a.Key = getenv(a.KeyEnv)
		if len(a.Key) < minKeyLength || keys[a.Key] {
			return nil, fmt.Errorf("account %q: %s must hold a unique key of at least %d characters", a.ID, a.KeyEnv, minKeyLength)
		}
		keys[a.Key] = true
		if a.WebhookURLEnv != "" {
			a.WebhookURL = getenv(a.WebhookURLEnv)
		}
		if a.WebhookSecretEnv != "" {
			a.WebhookSecret = getenv(a.WebhookSecretEnv)
		}
		if a.WebhookURL != "" && a.WebhookSecret == "" {
			return nil, fmt.Errorf("account %q: a webhook endpoint needs %s", a.ID, a.WebhookSecretEnv)
		}
	}
	return file.Accounts, nil
}

func LoadRates(path string) ([]Rate, error) {
	var file struct {
		Rates []Rate `yaml:"rates"`
	}
	if err := decode(path, &file); err != nil {
		return nil, err
	}
	seen := make(map[[2]string]bool)
	for i := range file.Rates {
		r := &file.Rates[i]
		value, err := strconv.ParseInt(r.Raw, 10, 64)
		if err != nil || value <= 0 || !currencyPattern.MatchString(r.Sell) || !currencyPattern.MatchString(r.Buy) || r.Sell == r.Buy {
			return nil, fmt.Errorf("rate %s/%s %q is invalid", r.Sell, r.Buy, r.Raw)
		}
		pair := [2]string{r.Sell, r.Buy}
		if seen[pair] {
			return nil, fmt.Errorf("rate %s/%s is declared twice", r.Sell, r.Buy)
		}
		seen[pair] = true
		r.RateE8 = value
	}
	return file.Rates, nil
}

func decode(path string, into any) error {
	data, err := os.ReadFile(path) //nolint:gosec // G304: the path is operator configuration, never request input
	if err != nil {
		return err
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(into); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}
