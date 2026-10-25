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
	"testing"
	"time"
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
	a, err := newApp(cfg, slog.New(slog.DiscardHandler), http.DefaultClient)
	if err != nil {
		t.Fatalf("newApp: %v", err)
	}
	srv := httptest.NewServer(a.handler)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || strings.TrimSpace(string(body)) != "ok" {
		t.Errorf("healthz = %d %q", resp.StatusCode, body)
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
