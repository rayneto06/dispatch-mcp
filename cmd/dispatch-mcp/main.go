// Command dispatch-mcp is an MCP server that lets an AI agent quote, book and
// track last-mile deliveries across several fleet partners.
//
// It speaks MCP over stdio (local use, e.g. Claude Code) or over stateless
// Streamable HTTP (hosted), and receives partner webhooks over HTTP.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/time/rate"

	"github.com/rayneto06/dispatch-mcp/internal/httpx"
	"github.com/rayneto06/dispatch-mcp/internal/obs"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, os.Getenv, os.Stdin, os.Stdout, os.Stderr)
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, "dispatch-mcp:", err)
		os.Exit(1)
	}
}

type config struct {
	Transport     string // "stdio" or "http"
	Addr          string // http: the MCP+webhook listener; stdio: the webhook listener
	MCPToken      string // secret path segment of the hosted MCP endpoint
	PartnerAPIKey string
	WebhookSecret string
	FleetBaseURL  string
	Partners      []string
	OSRMBaseURL   string
	QuoteDeadline time.Duration
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
		Transport:     get("TRANSPORT", "stdio"),
		MCPToken:      getenv("MCP_TOKEN"),
		PartnerAPIKey: getenv("PARTNER_API_KEY"),
		WebhookSecret: getenv("WEBHOOK_SECRET"),
		FleetBaseURL:  strings.TrimRight(get("FLEET_BASE_URL", "http://127.0.0.1:8090"), "/"),
		OSRMBaseURL:   strings.TrimRight(get("OSRM_BASE_URL", "https://router.project-osrm.org"), "/"),
	}
	for _, p := range strings.Split(get("PARTNERS", "alpha,beta,gamma"), ",") {
		if p = strings.TrimSpace(p); p != "" {
			cfg.Partners = append(cfg.Partners, p)
		}
	}

	var err error
	if cfg.QuoteDeadline, err = time.ParseDuration(get("QUOTE_DEADLINE", "1.5s")); err != nil {
		return config{}, fmt.Errorf("QUOTE_DEADLINE: %w", err)
	}
	if cfg.LogLevel, err = obs.ParseLevel(getenv("LOG_LEVEL")); err != nil {
		return config{}, fmt.Errorf("LOG_LEVEL: %w", err)
	}

	switch cfg.Transport {
	case "stdio":
		cfg.Addr = get("WEBHOOK_ADDR", "127.0.0.1:8081")
	case "http":
		cfg.Addr = ":" + get("PORT", "8080")
		if len(cfg.MCPToken) < 32 {
			return config{}, errors.New("MCP_TOKEN must be at least 32 characters in http mode")
		}
	default:
		return config{}, fmt.Errorf("TRANSPORT must be stdio or http, got %q", cfg.Transport)
	}

	if cfg.PartnerAPIKey == "" {
		return config{}, errors.New("PARTNER_API_KEY is required")
	}
	if cfg.WebhookSecret == "" {
		return config{}, errors.New("WEBHOOK_SECRET is required")
	}
	if len(cfg.Partners) == 0 {
		return config{}, errors.New("PARTNERS must name at least one partner")
	}
	return cfg, nil
}

// app is everything run needs, built once so tests can wire it the same way.
type app struct {
	mcp     *mcp.Server
	handler http.Handler // webhooks and health; plus the MCP endpoint in http mode
}

func newApp(cfg config, logger *slog.Logger, httpClient *http.Client) (*app, error) {
	a := &app{
		mcp: mcp.NewServer(&mcp.Implementation{Name: "dispatch-mcp", Version: "0.1.0"}, &mcp.ServerOptions{Logger: logger}),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok\n")
	})
	if cfg.Transport == "http" {
		// Stateless: no Mcp-Session-Id, so a Cloud Run restart never strands a
		// client with a dead session. Every request gets a fresh one.
		mcpHandler := mcp.NewStreamableHTTPHandler(
			func(*http.Request) *mcp.Server { return a.mcp },
			&mcp.StreamableHTTPOptions{Stateless: true, PropagateRequestCancellation: true, Logger: logger},
		)
		limiter := rate.NewLimiter(5, 20) // 5 req/s sustained, bursts of 20
		mux.Handle("/mcp/{token}", httpx.RequireToken(cfg.MCPToken, httpx.RateLimit(limiter, mcpHandler)))
	}
	a.handler = mux
	return a, nil
}

func run(ctx context.Context, getenv func(string) string, stdin io.Reader, stdout, stderr io.Writer) error {
	cfg, err := loadConfig(getenv)
	if err != nil {
		return err
	}
	// Logs go to stderr only: in stdio mode stdout is the MCP channel.
	logger := obs.NewLogger(stderr, cfg.LogLevel)
	a, err := newApp(cfg, logger, &http.Client{Timeout: 10 * time.Second})
	if err != nil {
		return err
	}
	logger.Info("starting", "transport", cfg.Transport, "addr", cfg.Addr, "partners", cfg.Partners)

	if cfg.Transport == "http" {
		return httpx.Run(ctx, httpx.NewServer(cfg.Addr, a.handler), 8*time.Second)
	}

	// stdio: MCP on stdin/stdout, webhooks on a local listener. Whichever
	// stops first (client closed stdin, signal, listener failed) stops both.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	httpErr := make(chan error, 1)
	go func() {
		err := httpx.Run(ctx, httpx.NewServer(cfg.Addr, a.handler), 8*time.Second)
		if err != nil {
			cancel()
		}
		httpErr <- err
	}()

	mcpErr := a.mcp.Run(ctx, &mcp.IOTransport{Reader: io.NopCloser(stdin), Writer: nopWriteCloser{stdout}})
	cancel()
	if err := <-httpErr; err != nil {
		return fmt.Errorf("webhook listener: %w", err)
	}
	if mcpErr != nil && !errors.Is(mcpErr, context.Canceled) {
		return fmt.Errorf("mcp server: %w", mcpErr)
	}
	logger.Info("stopped")
	return nil
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }
