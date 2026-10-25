// Command fakefleet simulates three fleet partners (alpha, beta, gamma) with
// their own REST APIs, failure modes and signed status webhooks.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/rayneto06/dispatch-mcp/internal/fleet"
	"github.com/rayneto06/dispatch-mcp/internal/httpx"
	"github.com/rayneto06/dispatch-mcp/internal/obs"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, os.Getenv, os.Stderr)
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, "fakefleet:", err)
		os.Exit(1)
	}
}

type config struct {
	Addr          string
	APIKey        string
	WebhookSecret string
	WebhookURL    string  // base URL of dispatch-mcp; events go to <WebhookURL>/webhooks/<partner>
	TimeScale     float64 // multiplies status timings; 0.1 runs ten times faster
	LogLevel      slog.Level
}

func loadConfig(getenv func(string) string) (config, error) {
	get := func(key, def string) string {
		if v := getenv(key); v != "" {
			return v
		}
		return def
	}

	cfg := config{
		Addr:          ":" + get("PORT", "8090"),
		APIKey:        getenv("PARTNER_API_KEY"),
		WebhookSecret: getenv("WEBHOOK_SECRET"),
		WebhookURL:    strings.TrimRight(get("WEBHOOK_URL", "http://127.0.0.1:8081"), "/"),
	}
	var err error
	if cfg.TimeScale, err = strconv.ParseFloat(get("TIME_SCALE", "1"), 64); err != nil || cfg.TimeScale <= 0 {
		return config{}, fmt.Errorf("TIME_SCALE must be a positive number, got %q", getenv("TIME_SCALE"))
	}
	if cfg.LogLevel, err = obs.ParseLevel(getenv("LOG_LEVEL")); err != nil {
		return config{}, fmt.Errorf("LOG_LEVEL: %w", err)
	}
	if cfg.APIKey == "" {
		return config{}, errors.New("PARTNER_API_KEY is required")
	}
	if cfg.WebhookSecret == "" {
		return config{}, errors.New("WEBHOOK_SECRET is required")
	}
	return cfg, nil
}

func run(ctx context.Context, getenv func(string) string, stderr io.Writer) error {
	cfg, err := loadConfig(getenv)
	if err != nil {
		return err
	}
	logger := obs.NewLogger(stderr, cfg.LogLevel)

	t := fleet.DefaultTimings()
	scale := func(d time.Duration) time.Duration { return time.Duration(float64(d) * cfg.TimeScale) }
	f := fleet.New(fleet.Config{
		APIKey:        cfg.APIKey,
		WebhookSecret: []byte(cfg.WebhookSecret),
		WebhookURL:    cfg.WebhookURL,
		Timings:       fleet.Timings{Assigned: scale(t.Assigned), PickedUp: scale(t.PickedUp), Delivered: scale(t.Delivered)},
		Jitter:        0.2,
		Logger:        logger,
	}, fleet.DefaultProfiles())
	f.Start()

	logger.Info("starting", "addr", cfg.Addr, "webhook_url", cfg.WebhookURL)
	err = httpx.Run(ctx, httpx.NewServer(cfg.Addr, f.Handler()), 8*time.Second)

	// The HTTP server has stopped; give queued webhooks the rest of the
	// shutdown window (Cloud Run allows about 10 s after SIGTERM).
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if serr := f.Shutdown(shutdownCtx); serr != nil {
		logger.Warn("webhooks left undelivered at shutdown", "error", serr)
	}
	return err
}
