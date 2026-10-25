// Package partner is the REST client dispatch-mcp uses to talk to one fleet
// partner. It owns the integration rules: which requests may be retried,
// how long to wait between attempts, and how failures are reported.
package partner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"time"

	"github.com/rayneto06/dispatch-mcp/internal/geo"
	"github.com/rayneto06/dispatch-mcp/internal/health"
	"github.com/rayneto06/dispatch-mcp/internal/obs"
)

// Wire types of the partner API, as this client sees it.

type QuoteRequest struct {
	Pickup    geo.Point `json:"pickup"`
	Dropoff   geo.Point `json:"dropoff"`
	DistanceM float64   `json:"distance_m"`
}

type Quote struct {
	PriceCents int
	Currency   string
	PickupETA  time.Duration
	ValidFor   time.Duration
}

type CreateRequest struct {
	Pickup          geo.Point `json:"pickup"`
	Dropoff         geo.Point `json:"dropoff"`
	PriceCents      int       `json:"price_cents"`
	ClientReference string    `json:"client_reference"`
}

type Delivery struct {
	ID        string    `json:"id"`
	Status    string    `json:"status"`
	Sequence  int64     `json:"sequence"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type Config struct {
	Name          string
	BaseURL       string // e.g. https://fleet.example/alpha/v1
	APIKey        string
	HTTPClient    *http.Client
	MaxAttempts   int           // default 3
	BaseDelay     time.Duration // default 200ms; doubles per retry
	MaxDelay      time.Duration // default 2s
	MaxRetryAfter time.Duration // default 5s; longer Retry-After values are capped
	Sleep         func(ctx context.Context, d time.Duration) error
	Counters      *health.Counters
	Logger        *slog.Logger
}

type Client struct {
	cfg Config
}

func New(cfg Config) *Client {
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 10 * time.Second}
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = 3
	}
	if cfg.BaseDelay <= 0 {
		cfg.BaseDelay = 200 * time.Millisecond
	}
	if cfg.MaxDelay <= 0 {
		cfg.MaxDelay = 2 * time.Second
	}
	if cfg.MaxRetryAfter <= 0 {
		cfg.MaxRetryAfter = 5 * time.Second
	}
	if cfg.Sleep == nil {
		cfg.Sleep = sleep
	}
	if cfg.Counters == nil {
		cfg.Counters = &health.Counters{} // counted, just not reported
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	return &Client{cfg: cfg}
}

func (c *Client) Name() string { return c.cfg.Name }

func (c *Client) Quote(ctx context.Context, req QuoteRequest) (Quote, error) {
	c.cfg.Counters.QuoteRequests.Add(1)
	var wire struct {
		PriceCents int    `json:"price_cents"`
		Currency   string `json:"currency"`
		PickupETAS int    `json:"pickup_eta_s"`
		ValidForS  int    `json:"valid_for_s"`
	}
	// A quote has no side effects at the partner, so retrying is safe.
	err := c.do(ctx, request{method: http.MethodPost, path: "/quotes", body: req, retrySafe: true}, &wire)
	if err != nil {
		return Quote{}, err
	}
	c.cfg.Counters.QuotesReturned.Add(1)
	return Quote{
		PriceCents: wire.PriceCents,
		Currency:   wire.Currency,
		PickupETA:  time.Duration(wire.PickupETAS) * time.Second,
		ValidFor:   time.Duration(wire.ValidForS) * time.Second,
	}, nil
}

// CreateDelivery books a delivery. The idempotency key must be the same on
// every attempt of one logical booking: it is what lets the partner turn a
// retry after a lost response into a no-op. Without a key there is no retry.
func (c *Client) CreateDelivery(ctx context.Context, idempotencyKey string, req CreateRequest) (Delivery, error) {
	c.cfg.Counters.CreateRequests.Add(1)
	var d Delivery
	err := c.do(ctx, request{
		method:         http.MethodPost,
		path:           "/deliveries",
		body:           req,
		idempotencyKey: idempotencyKey,
		retrySafe:      idempotencyKey != "",
	}, &d)
	return d, err
}

func (c *Client) GetDelivery(ctx context.Context, id string) (Delivery, error) {
	c.cfg.Counters.Polls.Add(1)
	var d Delivery
	err := c.do(ctx, request{method: http.MethodGet, path: "/deliveries/" + url.PathEscape(id), retrySafe: true}, &d)
	return d, err
}

// Ping hits the partner's health endpoint once. It is used to wake a
// scaled-to-zero partner, so it stays out of the counters.
func (c *Client) Ping(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.cfg.BaseURL+"/health", nil)
	if err != nil {
		return err
	}
	resp, err := c.cfg.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("partner %s: health returned %d", c.cfg.Name, resp.StatusCode)
	}
	return nil
}

type request struct {
	method, path   string
	body           any
	idempotencyKey string
	retrySafe      bool // may this request be sent more than once?
}

// do sends r, retrying only when it is both safe (r.retrySafe) and useful
// (429, 5xx, timeouts, connection errors), and never past ctx's deadline.
func (c *Client) do(ctx context.Context, r request, out any) error {
	var payload []byte
	if r.body != nil {
		var err error
		if payload, err = json.Marshal(r.body); err != nil {
			return fmt.Errorf("partner %s: encode request: %w", c.cfg.Name, err)
		}
	}
	maxAttempts := c.cfg.MaxAttempts
	if !r.retrySafe {
		maxAttempts = 1
	}

	for attempt := 1; ; attempt++ {
		status, header, body, err := c.attempt(ctx, r, payload, attempt)
		if err == nil && status < 300 {
			if out == nil {
				return nil
			}
			if err := json.Unmarshal(body, out); err != nil {
				return fmt.Errorf("partner %s: decode response: %w", c.cfg.Name, err)
			}
			return nil
		}

		apiErr := &APIError{Partner: c.cfg.Name, StatusCode: status, Attempts: attempt, Err: err}
		switch {
		case status == http.StatusTooManyRequests || status == http.StatusServiceUnavailable:
			apiErr.RetryAfter = parseRetryAfter(header.Get("Retry-After"), time.Now())
		case status >= 400 && status < 500:
			apiErr.Body = truncate(body, 200)
		}

		retryable := status == http.StatusTooManyRequests || status >= 500 || (err != nil && ctx.Err() == nil)
		if !retryable || attempt >= maxAttempts {
			return c.fail(apiErr)
		}

		wait := c.backoff(attempt)
		if apiErr.RetryAfter > 0 {
			wait = min(apiErr.RetryAfter, c.cfg.MaxRetryAfter)
		}
		// Honoring Retry-After must not blow the caller's deadline: if the
		// wait ends after it, fail now and let the caller decide.
		if deadline, ok := ctx.Deadline(); ok && time.Now().Add(wait).After(deadline) {
			return c.fail(apiErr)
		}
		c.cfg.Counters.Retries.Add(1)
		if err := c.cfg.Sleep(ctx, wait); err != nil {
			apiErr.Err = err
			return c.fail(apiErr)
		}
	}
}

func (c *Client) attempt(ctx context.Context, r request, payload []byte, n int) (int, http.Header, []byte, error) {
	var body io.Reader
	if payload != nil {
		body = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, r.method, c.cfg.BaseURL+r.path, body)
	if err != nil {
		return 0, nil, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if r.idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", r.idempotencyKey)
	}
	if id := obs.RequestID(ctx); id != "" {
		req.Header.Set(obs.RequestIDHeader, id)
	}

	c.cfg.Counters.Attempts.Add(1)
	start := time.Now()
	resp, err := c.cfg.HTTPClient.Do(req)
	status := 0
	var respBody []byte
	var header http.Header
	if err == nil {
		status, header = resp.StatusCode, resp.Header
		respBody, err = io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
	}

	switch {
	case status == http.StatusTooManyRequests:
		c.cfg.Counters.RateLimited.Add(1)
	case status >= 500:
		c.cfg.Counters.ServerErrors.Add(1)
	case err != nil && isTimeout(err):
		c.cfg.Counters.Timeouts.Add(1)
	}
	attrs := []any{"partner", c.cfg.Name, "method", r.method, "path", r.path, "status", status,
		"attempt", n, "duration_ms", time.Since(start).Milliseconds(), "request_id", obs.RequestID(ctx)}
	if err != nil {
		c.cfg.Logger.Warn("partner call", append(attrs, "error", err)...)
	} else {
		c.cfg.Logger.Info("partner call", attrs...)
	}
	return status, header, respBody, err
}

func (c *Client) fail(err *APIError) error {
	c.cfg.Counters.RecordError(err.Error(), time.Now())
	return err
}

// backoff is "full jitter": a random wait between 0 and an exponentially
// growing cap, so many clients retrying at once don't move in lockstep.
func (c *Client) backoff(attempt int) time.Duration {
	limit := min(c.cfg.MaxDelay, c.cfg.BaseDelay<<(attempt-1))
	return time.Duration(rand.Int64N(int64(limit) + 1))
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func truncate(b []byte, n int) string {
	if len(b) > n {
		b = b[:n]
	}
	return string(b)
}
