package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/xJohnNascimento/ditto/internal/api"
	"github.com/xJohnNascimento/ditto/internal/config"
	"github.com/xJohnNascimento/ditto/internal/provider"
	"github.com/xJohnNascimento/ditto/migrations"
	"github.com/xJohnNascimento/ditto/openapi"
)

const usage = "usage: ditto serve | migrate | scenarios | healthcheck"

var profiles = []string{"local", "ci", "sandbox"}

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "ditto:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New(usage)
	}
	switch args[0] {
	case "serve":
		return serve()
	case "migrate":
		return migrate(os.Getenv("DITTO_DATABASE_URL"))
	case "scenarios":
		for _, entry := range provider.Catalogue() {
			fmt.Printf("%-26s targets %s\n", entry.Fault, strings.Join(entry.Targets, ", "))
		}
		return nil
	case "healthcheck":
		return healthcheck(envOr("DITTO_HEALTH_URL", "http://127.0.0.1:9002/healthz"))
	}
	return fmt.Errorf("unknown command %q; %s", args[0], usage)
}

func serve() error {
	profile := os.Getenv("DITTO_PROFILE")
	if !slices.Contains(profiles, profile) {
		return fmt.Errorf("DITTO_PROFILE must be one of %v; Ditto never runs in production", profiles)
	}
	databaseURL := os.Getenv("DITTO_DATABASE_URL")
	adminKey := os.Getenv("DITTO_ADMIN_KEY")
	if databaseURL == "" || len(adminKey) < 32 {
		return errors.New("DITTO_DATABASE_URL and a DITTO_ADMIN_KEY of at least 32 characters are required")
	}
	accounts, err := config.LoadAccounts(envOr("DITTO_ACCOUNTS_FILE", "/etc/ditto/accounts.yaml"), os.Getenv)
	if err != nil {
		return err
	}
	for _, a := range accounts {
		if a.Key == adminKey {
			return fmt.Errorf("account %q uses the admin key", a.ID)
		}
	}
	rates, err := config.LoadRates(envOr("DITTO_RATES_FILE", "/etc/ditto/rates.yaml"))
	if err != nil {
		return err
	}
	opts, err := options()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	service := provider.New(pool, opts)
	if err := service.Bootstrap(ctx, accounts, rates); err != nil {
		return err
	}
	handler, err := api.NewHandler(openapi.Spec, api.Config{
		Service:   service,
		Accounts:  accounts,
		AdminKey:  adminKey,
		Profile:   profile,
		DatasetID: os.Getenv("DITTO_DATASET_ID"),
	})
	if err != nil {
		return err
	}
	addr := envOr("DITTO_ADDR", ":9002")
	srv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	workerCtx, stopWorkers := context.WithCancel(ctx)
	defer stopWorkers()
	var workers sync.WaitGroup
	workers.Go(func() { service.RunScheduler(workerCtx, 200*time.Millisecond) })
	workers.Go(func() { service.RunWebhooks(workerCtx, &http.Client{Timeout: 5 * time.Second}, 500*time.Millisecond) })
	errs := make(chan error, 1)
	go func() {
		errs <- srv.ListenAndServe()
	}()
	slog.Info("ditto listening", "addr", addr, "profile", profile)

	var result error
	select {
	case result = <-errs:
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		result = srv.Shutdown(shutdownCtx)
	}
	// The workers write through the pool, which the deferred Close below shuts down.
	stopWorkers()
	workers.Wait()
	return result
}

func options() (provider.Options, error) {
	ttl, err := time.ParseDuration(envOr("DITTO_QUOTE_TTL", "60s"))
	if err != nil {
		return provider.Options{}, fmt.Errorf("DITTO_QUOTE_TTL: %w", err)
	}
	rate, err := strconv.ParseFloat(envOr("DITTO_RATE_PER_SECOND", "10"), 64)
	if err != nil || rate <= 0 {
		return provider.Options{}, errors.New("DITTO_RATE_PER_SECOND must be positive")
	}
	burst, err := strconv.Atoi(envOr("DITTO_BURST", "20"))
	if err != nil || burst <= 0 {
		return provider.Options{}, errors.New("DITTO_BURST must be positive")
	}
	step, err := time.ParseDuration(envOr("DITTO_PAYOUT_STEP", "2s"))
	if err != nil || step < 0 {
		return provider.Options{}, errors.New("DITTO_PAYOUT_STEP must be a non-negative duration")
	}
	window, err := time.ParseDuration(envOr("DITTO_WEBHOOK_RETRY_WINDOW", "72h"))
	if err != nil || window <= 0 {
		return provider.Options{}, errors.New("DITTO_WEBHOOK_RETRY_WINDOW must be a positive duration")
	}
	return provider.Options{
		QuoteTTL: ttl, RatePerSecond: rate, Burst: burst, PayoutStep: step,
		WebhookBackoff: 2 * time.Second, WebhookWindow: window,
	}, nil
}

func migrate(databaseURL string) error {
	if databaseURL == "" {
		return errors.New("DITTO_DATABASE_URL is required")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	applied, err := migrations.Up(ctx, db)
	if err != nil {
		return err
	}
	slog.Info("migrations applied", "versions", applied)
	return nil
}

func healthcheck(url string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("health check returned %s", resp.Status)
	}
	return nil
}

func envOr(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok {
		return value
	}
	return fallback
}
