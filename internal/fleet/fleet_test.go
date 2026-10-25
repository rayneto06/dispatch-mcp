package fleet

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/rayneto06/dispatch-mcp/internal/webhooksig"
)

const (
	testKey    = "test-key"
	testSecret = "test-secret"
)

var fastTimings = Timings{Assigned: 20 * time.Millisecond, PickedUp: 40 * time.Millisecond, Delivered: 60 * time.Millisecond}

// receiver is a fake dispatch-mcp webhook endpoint that records verified events.
type receiver struct {
	mu     sync.Mutex
	events []event
	bad    int
}

func (rc *receiver) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	if err := webhooksig.Verify([]byte(testSecret), r.Header.Get(webhooksig.Header), body, time.Now(), time.Minute); err != nil {
		rc.mu.Lock()
		rc.bad++
		rc.mu.Unlock()
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	var ev event
	if err := json.Unmarshal(body, &ev); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	rc.mu.Lock()
	rc.events = append(rc.events, ev)
	rc.mu.Unlock()
}

func (rc *receiver) snapshot() []event {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return append([]event(nil), rc.events...)
}

type harness struct {
	fleet *Fleet
	api   *httptest.Server
	recv  *receiver
}

func newHarness(t *testing.T, profiles ...Profile) *harness {
	t.Helper()
	if len(profiles) == 0 {
		profiles = []Profile{{Name: "alpha", BaseCents: 699, PerKmCents: 150}}
	}
	recv := &receiver{}
	hooks := httptest.NewServer(recv)
	t.Cleanup(hooks.Close)

	f := New(Config{
		APIKey:        testKey,
		WebhookSecret: []byte(testSecret),
		WebhookURL:    hooks.URL,
		Timings:       fastTimings,
	}, profiles)
	f.Start()
	api := httptest.NewServer(f.Handler())
	t.Cleanup(func() {
		api.Close()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		f.Shutdown(ctx)
	})
	return &harness{fleet: f, api: api, recv: recv}
}

func (h *harness) do(t *testing.T, method, path, key string, body any, headers map[string]string) (*http.Response, []byte) {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, h.api.URL+path, r)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

var sampleQuote = quoteRequest{DistanceM: 10000}

func sampleCreate(price int) createRequest {
	return createRequest{PriceCents: price, ClientReference: "q_1"}
}

func TestAuthRequired(t *testing.T) {
	h := newHarness(t)
	for _, key := range []string{"", "wrong"} {
		if resp, _ := h.do(t, "POST", "/alpha/v1/quotes", key, sampleQuote, nil); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("key %q: status = %d, want 401", key, resp.StatusCode)
		}
	}
	if resp, _ := h.do(t, "GET", "/alpha/v1/health", "", nil, nil); resp.StatusCode != http.StatusOK {
		t.Errorf("partner health = %d, want 200 without auth", resp.StatusCode)
	}
	if resp, _ := h.do(t, "GET", "/health", "", nil, nil); resp.StatusCode != http.StatusOK {
		t.Errorf("root health = %d, want 200", resp.StatusCode)
	}
}

func TestQuotePrice(t *testing.T) {
	h := newHarness(t)
	resp, body := h.do(t, "POST", "/alpha/v1/quotes", testKey, sampleQuote, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, body)
	}
	var q quoteResponse
	json.Unmarshal(body, &q)
	if q.PriceCents != 699+1500 || q.Currency != "USD" || q.ValidForS != 600 {
		t.Errorf("quote = %+v, want 2199 USD valid 600s", q)
	}
}

func TestUnknownPartner(t *testing.T) {
	h := newHarness(t)
	if resp, _ := h.do(t, "POST", "/delta/v1/quotes", testKey, sampleQuote, nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestCreateIdempotency(t *testing.T) {
	h := newHarness(t)
	key := map[string]string{"Idempotency-Key": "q_1"}

	resp, body := h.do(t, "POST", "/alpha/v1/deliveries", testKey, sampleCreate(2199), key)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("first create = %d: %s", resp.StatusCode, body)
	}
	var first deliveryResponse
	json.Unmarshal(body, &first)
	if first.Status != "pending" || first.Sequence != 1 || first.ID == "" {
		t.Errorf("first = %+v, want pending seq 1", first)
	}

	resp, body = h.do(t, "POST", "/alpha/v1/deliveries", testKey, sampleCreate(2199), key)
	var again deliveryResponse
	json.Unmarshal(body, &again)
	if resp.StatusCode != http.StatusCreated || again.ID != first.ID || resp.Header.Get("Idempotent-Replayed") != "true" {
		t.Errorf("replay = %d %+v replayed=%q, want 201 same id replayed=true", resp.StatusCode, again, resp.Header.Get("Idempotent-Replayed"))
	}

	if resp, _ := h.do(t, "POST", "/alpha/v1/deliveries", testKey, sampleCreate(9999), key); resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("same key, different body = %d, want 422", resp.StatusCode)
	}
	if resp, _ := h.do(t, "POST", "/alpha/v1/deliveries", testKey, sampleCreate(2199), nil); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("no key = %d, want 400", resp.StatusCode)
	}
}

func TestGetDelivery(t *testing.T) {
	h := newHarness(t)
	_, body := h.do(t, "POST", "/alpha/v1/deliveries", testKey, sampleCreate(2199), map[string]string{"Idempotency-Key": "q_1"})
	var created deliveryResponse
	json.Unmarshal(body, &created)

	resp, body := h.do(t, "GET", "/alpha/v1/deliveries/"+created.ID, testKey, nil, nil)
	var got deliveryResponse
	json.Unmarshal(body, &got)
	if resp.StatusCode != http.StatusOK || got.ID != created.ID {
		t.Errorf("get = %d %+v", resp.StatusCode, got)
	}
	if resp, _ := h.do(t, "GET", "/alpha/v1/deliveries/alpha_nope", testKey, nil, nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown = %d, want 404", resp.StatusCode)
	}
}

func waitFor(t *testing.T, d time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return cond()
}

func TestProgressionSendsSignedEvents(t *testing.T) {
	h := newHarness(t)
	_, body := h.do(t, "POST", "/alpha/v1/deliveries", testKey, sampleCreate(2199), map[string]string{"Idempotency-Key": "q_1"})
	var created deliveryResponse
	json.Unmarshal(body, &created)

	if !waitFor(t, 2*time.Second, func() bool { return len(h.recv.snapshot()) >= 3 }) {
		t.Fatalf("got %d events, want 3", len(h.recv.snapshot()))
	}
	want := []struct {
		status string
		seq    int64
	}{{"assigned", 2}, {"picked_up", 3}, {"delivered", 4}}
	events := h.recv.snapshot()
	for i, w := range want {
		ev := events[i]
		if ev.Status != w.status || ev.Sequence != w.seq || ev.PartnerDeliveryID != created.ID ||
			ev.Partner != "alpha" || ev.Type != "delivery.status_changed" || ev.ID == "" {
			t.Errorf("event %d = %+v, want %s seq %d", i, ev, w.status, w.seq)
		}
	}
	if h.recv.bad != 0 {
		t.Errorf("%d events failed signature verification", h.recv.bad)
	}

	_, body = h.do(t, "GET", "/alpha/v1/deliveries/"+created.ID, testKey, nil, nil)
	var got deliveryResponse
	json.Unmarshal(body, &got)
	if got.Status != "delivered" || got.Sequence != 4 {
		t.Errorf("final state = %+v, want delivered seq 4", got)
	}
}

func TestShutdownStopsProgression(t *testing.T) {
	h := newHarness(t)
	h.do(t, "POST", "/alpha/v1/deliveries", testKey, sampleCreate(2199), map[string]string{"Idempotency-Key": "q_1"})

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := h.fleet.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown = %v", err)
	}
	time.Sleep(100 * time.Millisecond) // longer than the whole fastTimings progression
	if n := len(h.recv.snapshot()); n != 0 {
		t.Errorf("%d events sent after shutdown, want 0", n)
	}
	if resp, _ := h.do(t, "POST", "/alpha/v1/deliveries", testKey, sampleCreate(2199), map[string]string{"Idempotency-Key": "q_2"}); resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("create after shutdown = %d, want 503", resp.StatusCode)
	}
}

func (h *harness) deliveryCount(name string) int {
	h.fleet.mu.Lock()
	defer h.fleet.mu.Unlock()
	return len(h.fleet.partners[name].deliveries)
}

func TestFaults(t *testing.T) {
	key := map[string]string{"Idempotency-Key": "q_1"}

	t.Run("429 with Retry-After, nothing stored", func(t *testing.T) {
		h := newHarness(t, Profile{Name: "alpha", BaseCents: 699, Rate429: 1})
		resp, _ := h.do(t, "POST", "/alpha/v1/deliveries", testKey, sampleCreate(2199), key)
		if resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") != "1" {
			t.Errorf("status = %d Retry-After = %q, want 429 and 1", resp.StatusCode, resp.Header.Get("Retry-After"))
		}
		if n := h.deliveryCount("alpha"); n != 0 {
			t.Errorf("%d deliveries stored, want 0", n)
		}
	})

	t.Run("503, nothing stored", func(t *testing.T) {
		h := newHarness(t, Profile{Name: "alpha", BaseCents: 699, Rate503: 1})
		if resp, _ := h.do(t, "POST", "/alpha/v1/quotes", testKey, sampleQuote, nil); resp.StatusCode != http.StatusServiceUnavailable {
			t.Errorf("quote status = %d, want 503", resp.StatusCode)
		}
		if resp, _ := h.do(t, "POST", "/alpha/v1/deliveries", testKey, sampleCreate(2199), key); resp.StatusCode != http.StatusServiceUnavailable {
			t.Errorf("create status = %d, want 503", resp.StatusCode)
		}
		if n := h.deliveryCount("alpha"); n != 0 {
			t.Errorf("%d deliveries stored, want 0", n)
		}
	})

	t.Run("commit then fail, retry replays", func(t *testing.T) {
		h := newHarness(t, Profile{Name: "alpha", BaseCents: 699, CommitThenFail: 1})
		if resp, _ := h.do(t, "POST", "/alpha/v1/deliveries", testKey, sampleCreate(2199), key); resp.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("first create = %d, want 503 after committing", resp.StatusCode)
		}
		resp, body := h.do(t, "POST", "/alpha/v1/deliveries", testKey, sampleCreate(2199), key)
		var d deliveryResponse
		json.Unmarshal(body, &d)
		if resp.StatusCode != http.StatusCreated || d.ID == "" || resp.Header.Get("Idempotent-Replayed") != "true" {
			t.Errorf("retry = %d %+v, want 201 replay of the stored delivery", resp.StatusCode, d)
		}
		if n := h.deliveryCount("alpha"); n != 1 {
			t.Errorf("%d deliveries stored, want exactly 1", n)
		}
	})
}

func TestDuplicateEvents(t *testing.T) {
	h := newHarness(t, Profile{Name: "alpha", BaseCents: 699, DuplicateRate: 1})
	h.do(t, "POST", "/alpha/v1/deliveries", testKey, sampleCreate(2199), map[string]string{"Idempotency-Key": "q_1"})

	if !waitFor(t, 2*time.Second, func() bool { return len(h.recv.snapshot()) >= 6 }) {
		t.Fatalf("got %d events, want 6 (3 statuses, each sent twice)", len(h.recv.snapshot()))
	}
	time.Sleep(50 * time.Millisecond) // no seventh should follow
	counts := map[string]int{}
	for _, ev := range h.recv.snapshot() {
		counts[ev.ID]++
	}
	if len(counts) != 3 {
		t.Errorf("%d distinct event IDs, want 3", len(counts))
	}
	for id, n := range counts {
		if n != 2 {
			t.Errorf("event %s delivered %d times, want 2", id, n)
		}
	}
}

func TestDefaultProfilesFaults(t *testing.T) {
	for _, p := range DefaultProfiles() {
		flaky := p.Rate429 > 0 || p.Rate503 > 0 || p.CommitThenFail > 0
		if (p.Name == "gamma") != flaky {
			t.Errorf("%s: flaky = %v; only gamma should inject faults", p.Name, flaky)
		}
		if p.DuplicateRate <= 0 {
			t.Errorf("%s: DuplicateRate = %v, want > 0 so dedup is visible", p.Name, p.DuplicateRate)
		}
	}
}
