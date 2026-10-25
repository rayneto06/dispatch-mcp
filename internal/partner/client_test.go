package partner

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rayneto06/dispatch-mcp/internal/health"
	"github.com/rayneto06/dispatch-mcp/internal/obs"
)

// step is one scripted partner response.
type step struct {
	status     int
	retryAfter string
	delay      time.Duration
}

type seenRequest struct {
	method, path string
	header       http.Header
}

// script serves steps in order (repeating the last one) and records requests.
type script struct {
	mu    sync.Mutex
	steps []step
	seen  []seenRequest
}

func (s *script) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	io.Copy(io.Discard, r.Body)
	s.mu.Lock()
	s.seen = append(s.seen, seenRequest{r.Method, r.URL.Path, r.Header.Clone()})
	st := s.steps[min(len(s.seen), len(s.steps))-1]
	s.mu.Unlock()

	if st.delay > 0 {
		select {
		case <-time.After(st.delay):
		case <-r.Context().Done():
			return
		}
	}
	if st.retryAfter != "" {
		w.Header().Set("Retry-After", st.retryAfter)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(st.status)
	if st.status < 300 {
		io.WriteString(w, `{"id":"alpha_1","status":"pending","sequence":1,"price_cents":2199,"currency":"USD","pickup_eta_s":600,"valid_for_s":600}`)
	} else {
		io.WriteString(w, `{"error":"scripted failure"}`)
	}
}

func (s *script) requests() []seenRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]seenRequest(nil), s.seen...)
}

type fixture struct {
	client   *Client
	script   *script
	sleeps   []time.Duration
	counters *health.Counters
}

func newFixture(t *testing.T, steps ...step) *fixture {
	t.Helper()
	f := &fixture{script: &script{steps: steps}, counters: &health.Counters{}}
	srv := httptest.NewServer(f.script)
	t.Cleanup(srv.Close)
	f.client = New(Config{
		Name:     "gamma",
		BaseURL:  srv.URL + "/gamma/v1",
		APIKey:   "k",
		Counters: f.counters,
		Logger:   slog.New(slog.DiscardHandler),
		Sleep: func(ctx context.Context, d time.Duration) error {
			f.sleeps = append(f.sleeps, d)
			return nil
		},
	})
	return f
}

var (
	quoteCall = func(ctx context.Context, c *Client) error {
		_, err := c.Quote(ctx, QuoteRequest{DistanceM: 1000})
		return err
	}
	createCall = func(ctx context.Context, c *Client) error {
		_, err := c.CreateDelivery(ctx, "q_1", CreateRequest{PriceCents: 2199})
		return err
	}
	createNoKey = func(ctx context.Context, c *Client) error {
		_, err := c.CreateDelivery(ctx, "", CreateRequest{PriceCents: 2199})
		return err
	}
	getCall = func(ctx context.Context, c *Client) error { _, err := c.GetDelivery(ctx, "alpha_1"); return err }
)

func TestRetryPolicy(t *testing.T) {
	tests := []struct {
		name         string
		call         func(context.Context, *Client) error
		steps        []step
		wantAttempts int
		wantIs       error // nil means success
		wantText     string
		wantSleeps   []time.Duration // nil: don't check exact values
	}{
		{"quote ok", quoteCall, []step{{status: 200}}, 1, nil, "", nil},
		{"quote 503 then ok", quoteCall, []step{{status: 503}, {status: 200}}, 2, nil, "", nil},
		{"quote 503 three times", quoteCall, []step{{status: 503}}, 3, ErrUnavailable, "partner gamma: unavailable (503) after 3 attempts", nil},
		{"quote 400 not retried", quoteCall, []step{{status: 400}}, 1, nil, "rejected (400)", nil},
		{"create with key 503 then ok", createCall, []step{{status: 503}, {status: 201}}, 2, nil, "", nil},
		{"create without key not retried", createNoKey, []step{{status: 503}}, 1, ErrUnavailable, "after 1 attempt", nil},
		{"get 404", getCall, []step{{status: 404}}, 1, ErrNotFound, "", nil},
		{"429 honors Retry-After", quoteCall, []step{{status: 429, retryAfter: "1"}, {status: 200}}, 2, nil, "", []time.Duration{time.Second}},
		{"Retry-After capped at 5s", quoteCall, []step{{status: 429, retryAfter: "30"}, {status: 200}}, 2, nil, "", []time.Duration{5 * time.Second}},
		{"429 exhausted", quoteCall, []step{{status: 429, retryAfter: "1"}}, 3, ErrRateLimited, "partner gamma: rate limited (429), retry after 1s", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t, tt.steps...)
			err := tt.call(context.Background(), f.client)

			if got := len(f.script.requests()); got != tt.wantAttempts {
				t.Errorf("attempts = %d, want %d", got, tt.wantAttempts)
			}
			switch {
			case tt.wantIs != nil && !errors.Is(err, tt.wantIs):
				t.Errorf("err = %v, want errors.Is %v", err, tt.wantIs)
			case tt.wantIs == nil && tt.wantText == "" && err != nil:
				t.Errorf("err = %v, want nil", err)
			}
			if tt.wantText != "" && (err == nil || !strings.Contains(err.Error(), tt.wantText)) {
				t.Errorf("err = %v, want text %q", err, tt.wantText)
			}
			if tt.wantText == "rejected (400)" && errors.Is(err, ErrUnavailable) {
				t.Errorf("a 400 must not count as unavailable")
			}
			if tt.wantSleeps != nil && !equalDurations(f.sleeps, tt.wantSleeps) {
				t.Errorf("sleeps = %v, want %v", f.sleeps, tt.wantSleeps)
			}
			if len(f.sleeps) != tt.wantAttempts-1 {
				t.Errorf("slept %d times for %d attempts", len(f.sleeps), tt.wantAttempts)
			}
		})
	}
}

func equalDurations(a, b []time.Duration) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestCreateRetriesReuseIdempotencyKey(t *testing.T) {
	f := newFixture(t, step{status: 503}, step{status: 201})
	if err := createCall(context.Background(), f.client); err != nil {
		t.Fatal(err)
	}
	reqs := f.script.requests()
	if len(reqs) != 2 || reqs[0].header.Get("Idempotency-Key") != "q_1" || reqs[1].header.Get("Idempotency-Key") != "q_1" {
		t.Errorf("idempotency keys = %q, %q; want q_1 on both attempts",
			reqs[0].header.Get("Idempotency-Key"), reqs[len(reqs)-1].header.Get("Idempotency-Key"))
	}
}

func TestBackoffIsBoundedJitter(t *testing.T) {
	f := newFixture(t, step{status: 503})
	quoteCall(context.Background(), f.client)
	for i, d := range f.sleeps {
		limit := min(2*time.Second, 200*time.Millisecond<<i)
		if d < 0 || d > limit {
			t.Errorf("sleep %d = %v, want within [0, %v]", i, d, limit)
		}
	}
}

func TestRetryAfterBeyondDeadlineFailsFast(t *testing.T) {
	script := &script{steps: []step{{status: 429, retryAfter: "1"}}}
	srv := httptest.NewServer(script)
	defer srv.Close()
	c := New(Config{Name: "gamma", BaseURL: srv.URL, APIKey: "k", Logger: slog.New(slog.DiscardHandler)}) // real Sleep

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := quoteCall(ctx, c)

	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Errorf("took %v, want a fast failure", elapsed)
	}
	var apiErr *APIError
	if !errors.Is(err, ErrRateLimited) || !errors.As(err, &apiErr) || apiErr.RetryAfter != time.Second {
		t.Errorf("err = %v, want rate limited with RetryAfter 1s", err)
	}
	if n := len(script.requests()); n != 1 {
		t.Errorf("attempts = %d, want 1", n)
	}
}

func TestContextCancelStopsRetries(t *testing.T) {
	script := &script{steps: []step{{status: 503}}}
	srv := httptest.NewServer(script)
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	c := New(Config{Name: "gamma", BaseURL: srv.URL, APIKey: "k", Logger: slog.New(slog.DiscardHandler),
		Sleep: func(ctx context.Context, d time.Duration) error {
			cancel() // the caller gives up while we wait
			<-ctx.Done()
			return ctx.Err()
		}})

	err := quoteCall(ctx, c)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if n := len(script.requests()); n != 1 {
		t.Errorf("attempts = %d, want 1", n)
	}
}

func TestPerAttemptTimeoutIsRetried(t *testing.T) {
	script := &script{steps: []step{{status: 200, delay: 200 * time.Millisecond}}}
	srv := httptest.NewServer(script)
	defer srv.Close()
	counters := &health.Counters{}
	c := New(Config{Name: "beta", BaseURL: srv.URL, APIKey: "k", Counters: counters,
		HTTPClient: &http.Client{Timeout: 30 * time.Millisecond},
		Logger:     slog.New(slog.DiscardHandler),
		Sleep:      func(context.Context, time.Duration) error { return nil }})

	err := quoteCall(context.Background(), c)
	if err == nil || !strings.Contains(err.Error(), "partner beta: timed out after 3 attempts") {
		t.Errorf("err = %v, want timed out after 3 attempts", err)
	}
	if errors.Is(err, ErrUnavailable) {
		t.Errorf("a timeout must not count as unavailable")
	}
	if got := counters.Timeouts.Load(); got != 3 {
		t.Errorf("timeouts = %d, want 3", got)
	}
}

func TestHeadersSent(t *testing.T) {
	f := newFixture(t, step{status: 201})
	ctx := obs.WithRequestID(context.Background(), "req_abc")
	if err := createCall(ctx, f.client); err != nil {
		t.Fatal(err)
	}
	r := f.script.requests()[0]
	if r.method != "POST" || r.path != "/gamma/v1/deliveries" {
		t.Errorf("request = %s %s", r.method, r.path)
	}
	for k, want := range map[string]string{
		"Authorization":   "Bearer k",
		"Idempotency-Key": "q_1",
		"X-Request-ID":    "req_abc",
		"Content-Type":    "application/json",
	} {
		if got := r.header.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
}

func TestCountersUpdated(t *testing.T) {
	f := newFixture(t, step{status: 503}, step{status: 200})
	if err := quoteCall(context.Background(), f.client); err != nil {
		t.Fatal(err)
	}
	c := f.counters
	got := []int64{c.Attempts.Load(), c.Retries.Load(), c.ServerErrors.Load(), c.QuoteRequests.Load(), c.QuotesReturned.Load()}
	want := []int64{2, 1, 1, 1, 1}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("counters [attempts retries 5xx quote_requests quotes_returned] = %v, want %v", got, want)
			break
		}
	}
}

func TestQuoteDecodes(t *testing.T) {
	f := newFixture(t, step{status: 200})
	q, err := f.client.Quote(context.Background(), QuoteRequest{DistanceM: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if q.PriceCents != 2199 || q.Currency != "USD" || q.PickupETA != 10*time.Minute || q.ValidFor != 10*time.Minute {
		t.Errorf("quote = %+v", q)
	}
}

func TestPing(t *testing.T) {
	f := newFixture(t, step{status: 200})
	if err := f.client.Ping(context.Background()); err != nil {
		t.Errorf("Ping = %v", err)
	}
	if r := f.script.requests()[0]; r.path != "/gamma/v1/health" {
		t.Errorf("ping path = %s", r.path)
	}
	if f.counters.Attempts.Load() != 0 {
		t.Errorf("ping must not count as a partner call")
	}
}
