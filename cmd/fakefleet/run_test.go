package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// statusRecorder is a webhook receiver that records which statuses arrived.
// Each request waits for gate (when set) and then for delay.
type statusRecorder struct {
	gate  chan struct{}
	delay time.Duration

	mu       sync.Mutex
	seen     map[string]bool
	requests int
}

func newStatusRecorder() *statusRecorder { return &statusRecorder{seen: map[string]bool{}} }

func (s *statusRecorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var ev struct {
		Status string `json:"status"`
	}
	json.NewDecoder(r.Body).Decode(&ev)
	s.mu.Lock()
	s.requests++
	s.mu.Unlock()
	if s.gate != nil {
		<-s.gate
	}
	time.Sleep(s.delay)
	s.mu.Lock()
	s.seen[ev.Status] = true
	s.mu.Unlock()
}

func (s *statusRecorder) delivered() map[string]bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]bool{}
	for k, v := range s.seen {
		out[k] = v
	}
	return out
}

func (s *statusRecorder) received() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requests
}

// startFleet runs the fakefleet command on a free port, sending webhooks to
// webhookURL with statuses a thousand times faster than real time.
func startFleet(t *testing.T, webhookURL string) (base string, stop context.CancelFunc, done <-chan error) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	vars := map[string]string{
		"PORT": strconv.Itoa(port), "PARTNER_API_KEY": "key", "WEBHOOK_SECRET": "secret",
		"WEBHOOK_URL": webhookURL, "TIME_SCALE": "0.001",
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	errc := make(chan error, 1)
	go func() { errc <- run(ctx, func(k string) string { return vars[k] }, io.Discard) }()

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	deadline := time.Now().Add(2 * time.Second)
	for {
		c, err := net.Dial("tcp", addr)
		if err == nil {
			c.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("fakefleet did not start: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	return "http://" + addr, cancel, errc
}

// bookOne books a delivery with partner alpha.
func bookOne(t *testing.T, base string) {
	t.Helper()
	body := `{"pickup":{"lat":37.7955,"lon":-122.3937},"dropoff":{"lat":37.7599,"lon":-122.4268},"price_cents":1500,"client_reference":"q_1"}`
	req, _ := http.NewRequest(http.MethodPost, base+"/alpha/v1/deliveries", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer key")
	req.Header.Set("Idempotency-Key", "q_1")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("booking: status %d", resp.StatusCode)
	}
}

func allStatuses(seen map[string]bool) bool {
	return seen["assigned"] && seen["picked_up"] && seen["delivered"]
}

// Queued webhooks get what is left of the shutdown window, not a fixed 2 s:
// a slow receiver here needs about 2.4 s for the last one.
func TestRunGivesWebhooksTheRestOfTheWindow(t *testing.T) {
	rec := newStatusRecorder()
	rec.delay = 1200 * time.Millisecond
	receiver := httptest.NewServer(rec)
	defer receiver.Close()

	base, stop, done := startFleet(t, receiver.URL)
	bookOne(t, base)
	time.Sleep(300 * time.Millisecond) // all three statuses are queued or in flight
	stop()

	if err := <-done; err != nil {
		t.Fatalf("run = %v", err)
	}
	if seen := rec.delivered(); !allStatuses(seen) {
		t.Errorf("delivered before exit: %v, want assigned, picked_up and delivered", seen)
	}
}

// Webhooks already on their way when the process is told to stop are still
// delivered before it exits.
func TestRunFlushesWebhooksOnShutdown(t *testing.T) {
	rec := newStatusRecorder()
	rec.gate = make(chan struct{})
	receiver := httptest.NewServer(rec)
	defer receiver.Close()

	base, stop, done := startFleet(t, receiver.URL)
	bookOne(t, base)
	time.Sleep(300 * time.Millisecond) // all three statuses happen within ~200 ms
	if rec.received() == 0 {
		t.Fatal("no webhook reached the receiver before shutdown")
	}
	stop()
	time.Sleep(50 * time.Millisecond)
	close(rec.gate)

	if err := <-done; err != nil {
		t.Fatalf("run = %v", err)
	}
	if seen := rec.delivered(); !allStatuses(seen) {
		t.Errorf("delivered before exit: %v, want assigned, picked_up and delivered", seen)
	}
}
