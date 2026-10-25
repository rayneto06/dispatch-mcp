package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/rayneto06/dispatch-mcp/internal/dispatch"
	"github.com/rayneto06/dispatch-mcp/internal/fleet"
	"github.com/rayneto06/dispatch-mcp/internal/geo"
	"github.com/rayneto06/dispatch-mcp/internal/health"
)

const testToken = "0123456789abcdef0123456789abcdef"

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func baseEnv() map[string]string {
	return map[string]string{
		"PARTNER_API_KEY": "key",
		"WEBHOOK_SECRET":  "secret",
	}
}

func with(m map[string]string, kv ...string) map[string]string {
	out := map[string]string{}
	for k, v := range m {
		out[k] = v
	}
	for i := 0; i+1 < len(kv); i += 2 {
		out[kv[i]] = kv[i+1]
	}
	return out
}

func TestLoadConfigDefaults(t *testing.T) {
	cfg, err := loadConfig(env(baseEnv()))
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.Transport != "stdio" || cfg.Addr != "127.0.0.1:8081" {
		t.Errorf("transport/addr = %q/%q", cfg.Transport, cfg.Addr)
	}
	if !reflect.DeepEqual(cfg.Partners, []string{"alpha", "beta", "gamma"}) {
		t.Errorf("partners = %v", cfg.Partners)
	}
	if cfg.QuoteDeadline != 1500*time.Millisecond {
		t.Errorf("quote deadline = %v", cfg.QuoteDeadline)
	}
	if cfg.OSRMBaseURL != "https://router.project-osrm.org" || cfg.FleetBaseURL != "http://127.0.0.1:8090" {
		t.Errorf("urls = %q %q", cfg.OSRMBaseURL, cfg.FleetBaseURL)
	}
	if cfg.LogLevel != slog.LevelInfo {
		t.Errorf("log level = %v", cfg.LogLevel)
	}
}

func TestLoadConfig(t *testing.T) {
	tests := []struct {
		name     string
		env      map[string]string
		wantErr  string
		wantAddr string
	}{
		{"http default port", with(baseEnv(), "TRANSPORT", "http", "MCP_TOKEN", testToken), "", ":8080"},
		{"http custom port", with(baseEnv(), "TRANSPORT", "http", "MCP_TOKEN", testToken, "PORT", "9000"), "", ":9000"},
		{"stdio webhook addr", with(baseEnv(), "WEBHOOK_ADDR", "127.0.0.1:9999"), "", "127.0.0.1:9999"},
		{"missing api key", with(baseEnv(), "PARTNER_API_KEY", ""), "PARTNER_API_KEY", ""},
		{"missing webhook secret", with(baseEnv(), "WEBHOOK_SECRET", ""), "WEBHOOK_SECRET", ""},
		{"http without token", with(baseEnv(), "TRANSPORT", "http"), "MCP_TOKEN", ""},
		{"http short token", with(baseEnv(), "TRANSPORT", "http", "MCP_TOKEN", "short"), "MCP_TOKEN", ""},
		{"bad transport", with(baseEnv(), "TRANSPORT", "carrier-pigeon"), "TRANSPORT", ""},
		{"bad deadline", with(baseEnv(), "QUOTE_DEADLINE", "soon"), "QUOTE_DEADLINE", ""},
		{"bad log level", with(baseEnv(), "LOG_LEVEL", "loud"), "LOG_LEVEL", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := loadConfig(env(tt.env))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want mention of %s", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("loadConfig: %v", err)
			}
			if cfg.Addr != tt.wantAddr {
				t.Errorf("addr = %q, want %q", cfg.Addr, tt.wantAddr)
			}
		})
	}
}

// syncBuffer is a bytes.Buffer safe for one writer goroutine and one reader.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestStdioStdoutIsOnlyJSONRPC(t *testing.T) {
	stdinR, stdinW := io.Pipe()
	var stdout, stderr syncBuffer
	done := make(chan error, 1)
	go func() {
		done <- run(context.Background(), env(with(baseEnv(), "WEBHOOK_ADDR", "127.0.0.1:0")), stdinR, &stdout, &stderr)
	}()

	initialize := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"0"}}}` + "\n"
	if _, err := io.WriteString(stdinW, initialize); err != nil {
		t.Fatalf("write stdin: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(stdout.String(), "\n") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	stdinW.Close()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return after stdin EOF")
	}

	out := stdout.String()
	if out == "" {
		t.Fatal("no response on stdout")
	}
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		if strings.TrimSpace(sc.Text()) == "" {
			continue
		}
		var msg map[string]any
		if err := json.Unmarshal(sc.Bytes(), &msg); err != nil || msg["jsonrpc"] != "2.0" {
			t.Errorf("stdout line is not JSON-RPC: %q", sc.Text())
		}
	}
	if !strings.Contains(stderr.String(), `"severity"`) {
		t.Errorf("expected a log line on stderr, got %q", stderr.String())
	}
}

func TestHTTPModeServesHealthAndMCP(t *testing.T) {
	cfg, err := loadConfig(env(with(baseEnv(), "TRANSPORT", "http", "MCP_TOKEN", testToken)))
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	a := newApp(cfg, slog.New(slog.DiscardHandler), http.DefaultClient)
	srv := httptest.NewServer(a.handler)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || strings.TrimSpace(string(body)) != "ok" {
		t.Errorf("health = %d %q", resp.StatusCode, body)
	}

	ping := func(path string) (int, string) {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+path, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	if code, body := ping("/mcp/" + testToken); code != http.StatusOK || !strings.Contains(body, `"result"`) {
		t.Errorf("ping with token = %d %q", code, body)
	}
	if code, _ := ping("/mcp/wrong"); code != http.StatusNotFound {
		t.Errorf("ping with wrong token = %d, want 404", code)
	}
}

// e2e wires the real app to a real (in-process) fleet and OSRM stand-in.
type e2e struct {
	app     *app
	fleet   *fleet.Fleet
	session *mcp.ClientSession
}

func newE2E(t *testing.T, profiles []fleet.Profile) *e2e {
	t.Helper()
	// The fleet needs dispatch's URL for webhooks and the app needs the
	// fleet's URL, so dispatch's server starts first with a handler that is
	// filled in once the app exists.
	var handler atomic.Pointer[http.Handler]
	dispatchSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		(*handler.Load()).ServeHTTP(w, r)
	}))
	t.Cleanup(dispatchSrv.Close)

	fl := fleet.New(fleet.Config{
		APIKey:        "key",
		WebhookSecret: []byte("secret"),
		WebhookURL:    dispatchSrv.URL,
		Timings:       fleet.Timings{Assigned: 50 * time.Millisecond, PickedUp: 100 * time.Millisecond, Delivered: 150 * time.Millisecond},
	}, profiles)
	fl.Start()
	fleetSrv := httptest.NewServer(fl.Handler())
	t.Cleanup(func() {
		fleetSrv.Close()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		fl.Shutdown(ctx)
	})

	osrm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"code":"Ok","routes":[{"distance":4321.5,"duration":610.2}]}`)
	}))
	t.Cleanup(osrm.Close)

	names := make([]string, len(profiles))
	for i, p := range profiles {
		names[i] = p.Name
	}
	cfg, err := loadConfig(env(with(baseEnv(), "FLEET_BASE_URL", fleetSrv.URL, "OSRM_BASE_URL", osrm.URL, "PARTNERS", strings.Join(names, ","))))
	if err != nil {
		t.Fatal(err)
	}
	a := newApp(cfg, slog.New(slog.DiscardHandler), &http.Client{Timeout: 5 * time.Second})
	h := a.handler
	handler.Store(&h)

	ct, st := mcp.NewInMemoryTransports()
	if _, err := a.mcp.Connect(context.Background(), st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "e2e", Version: "0"}, nil).Connect(context.Background(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return &e2e{app: a, fleet: fl, session: cs}
}

func (e *e2e) call(t *testing.T, tool string, args any, out any) {
	t.Helper()
	res, err := e.session.CallTool(context.Background(), &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	if res.IsError {
		var msg string
		for _, c := range res.Content {
			if tc, ok := c.(*mcp.TextContent); ok {
				msg += tc.Text
			}
		}
		t.Fatalf("%s returned a tool error: %s", tool, msg)
	}
	b, _ := json.Marshal(res.StructuredContent)
	if err := json.Unmarshal(b, out); err != nil {
		t.Fatalf("%s: decode %s: %v", tool, b, err)
	}
}

var (
	ferryBuilding  = geo.Point{Lat: 37.7955, Lon: -122.3937}
	missionDolores = geo.Point{Lat: 37.7599, Lon: -122.4268}
)

type created struct {
	Delivery      dispatch.Delivery `json:"delivery"`
	AlreadyBooked bool              `json:"already_booked"`
}

type fetched struct {
	Delivery dispatch.Delivery `json:"delivery"`
	Note     string            `json:"note"`
}

func (e *e2e) waitDelivered(t *testing.T, id string) dispatch.Delivery {
	t.Helper()
	var got fetched
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		e.call(t, "get_delivery", map[string]any{"delivery_id": id}, &got)
		if got.Delivery.Status == dispatch.StatusDelivered {
			return got.Delivery
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("delivery %s still %s after 3s", id, got.Delivery.Status)
	return dispatch.Delivery{}
}

func TestEndToEnd(t *testing.T) {
	e := newE2E(t, []fleet.Profile{
		{Name: "alpha", BaseCents: 699, PerKmCents: 150},
		{Name: "beta", BaseCents: 899, PerKmCents: 170},
		{Name: "gamma", BaseCents: 499, PerKmCents: 120},
	})

	var quoted dispatch.QuoteResult
	e.call(t, "quote_delivery", map[string]any{"pickup": ferryBuilding, "dropoff": missionDolores}, &quoted)
	if len(quoted.Quotes) != 3 || quoted.Quotes[0].Partner != "gamma" || quoted.Route.Source != "osrm" || quoted.Route.DistanceKm != 4.3 {
		t.Fatalf("quotes = %+v route = %+v", quoted.Quotes, quoted.Route)
	}

	var booked created
	e.call(t, "create_delivery", map[string]any{"quote_id": quoted.Quotes[0].ID}, &booked)
	if booked.AlreadyBooked || booked.Delivery.Status != dispatch.StatusPending || booked.Delivery.Partner != "gamma" {
		t.Fatalf("create = %+v", booked)
	}
	var again created
	e.call(t, "create_delivery", map[string]any{"quote_id": quoted.Quotes[0].ID}, &again)
	if !again.AlreadyBooked || again.Delivery.ID != booked.Delivery.ID {
		t.Errorf("second create = %+v, want already_booked with the same delivery_id", again)
	}

	d := e.waitDelivered(t, booked.Delivery.ID)
	sources := map[dispatch.Source]bool{}
	for _, h := range d.History {
		sources[h.Source] = true
	}
	if !sources[dispatch.SourceWebhook] || len(d.History) != 4 {
		t.Errorf("history = %+v, want create + 3 webhook updates", d.History)
	}

	var listed struct {
		Deliveries []dispatch.Delivery `json:"deliveries"`
	}
	e.call(t, "list_deliveries", map[string]any{}, &listed)
	if len(listed.Deliveries) != 1 {
		t.Errorf("list = %d deliveries, want 1", len(listed.Deliveries))
	}

	var snap health.Snapshot
	e.call(t, "partner_health", map[string]any{}, &snap)
	for _, p := range snap.Partners {
		if p.Partner == "gamma" && (p.WebhooksReceived < 3 || p.CreateRequests < 1) {
			t.Errorf("gamma health = %+v, want >= 3 webhooks and >= 1 create", p)
		}
	}
}

func TestEndToEndWithFlakyPartner(t *testing.T) {
	e := newE2E(t, []fleet.Profile{{Name: "gamma", BaseCents: 499, PerKmCents: 120, CommitThenFail: 1, DuplicateRate: 1}})

	var quoted dispatch.QuoteResult
	e.call(t, "quote_delivery", map[string]any{"pickup": ferryBuilding, "dropoff": missionDolores}, &quoted)
	// gamma commits the booking, then answers 503; the client retries with
	// the same Idempotency-Key and gets the stored booking back.
	var booked created
	e.call(t, "create_delivery", map[string]any{"quote_id": quoted.Quotes[0].ID}, &booked)
	if booked.AlreadyBooked || booked.Delivery.PartnerDeliveryID == "" {
		t.Fatalf("create = %+v", booked)
	}

	d := e.waitDelivered(t, booked.Delivery.ID)
	seen := map[dispatch.Status]int{}
	for _, h := range d.History {
		seen[h.Status]++
	}
	for status, n := range seen {
		if n != 1 {
			t.Errorf("status %s appears %d times in history; duplicates must be dropped", status, n)
		}
	}

	var snap health.Snapshot
	e.call(t, "partner_health", map[string]any{}, &snap)
	g := snap.Partners[0]
	if g.Retries < 1 || g.ServerErrors < 1 {
		t.Errorf("gamma retries = %d, 5xx = %d; want the commit-then-fail retry counted", g.Retries, g.ServerErrors)
	}
	// Webhooks can still be in flight when the status reads delivered.
	deadline := time.Now().Add(2 * time.Second)
	for g.DuplicatesDropped < 3 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		e.call(t, "partner_health", map[string]any{}, &snap)
		g = snap.Partners[0]
	}
	if g.DuplicatesDropped < 3 {
		t.Errorf("duplicates_dropped = %d, want >= 3", g.DuplicatesDropped)
	}
}
