package httpx

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"golang.org/x/time/rate"
)

var okHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
})

func TestRequireToken(t *testing.T) {
	const token = "0123456789abcdef0123456789abcdef"
	mux := http.NewServeMux()
	mux.Handle("/mcp/{token}", RequireToken(token, okHandler))

	tests := []struct {
		name string
		path string
		want int
	}{
		{"right token", "/mcp/" + token, http.StatusOK},
		{"wrong token", "/mcp/ffffffffffffffffffffffffffffffff", http.StatusNotFound},
		{"prefix of token", "/mcp/0123456789abcdef", http.StatusNotFound},
		{"token plus suffix", "/mcp/" + token + "00", http.StatusNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, tt.path, nil))
			if rec.Code != tt.want {
				t.Errorf("status = %d, want %d", rec.Code, tt.want)
			}
		})
	}
}

func TestRequireTokenEmptyPathValue(t *testing.T) {
	// Mounted without a {token} wildcard, PathValue is empty and must never match.
	h := RequireToken("0123456789abcdef0123456789abcdef", okHandler)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/mcp/", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestRateLimit(t *testing.T) {
	h := RateLimit(rate.NewLimiter(0, 2), okHandler)
	for i, want := range []int{http.StatusOK, http.StatusOK, http.StatusTooManyRequests} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", nil))
		if rec.Code != want {
			t.Fatalf("request %d: status = %d, want %d", i+1, rec.Code, want)
		}
		if want == http.StatusTooManyRequests && rec.Header().Get("Retry-After") != "1" {
			t.Errorf("Retry-After = %q, want 1", rec.Header().Get("Retry-After"))
		}
	}
}

func TestNewServerTimeouts(t *testing.T) {
	srv := NewServer(":0", okHandler)
	if srv.ReadHeaderTimeout != 5*time.Second || srv.ReadTimeout != 30*time.Second ||
		srv.WriteTimeout != 60*time.Second || srv.IdleTimeout != 120*time.Second {
		t.Errorf("timeouts = %v/%v/%v/%v", srv.ReadHeaderTimeout, srv.ReadTimeout, srv.WriteTimeout, srv.IdleTimeout)
	}
}

func TestRunShutsDownOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, NewServer("127.0.0.1:0", okHandler), time.Second) }()

	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned %v, want nil", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func TestRunReportsListenError(t *testing.T) {
	err := Run(context.Background(), NewServer("256.0.0.1:1", okHandler), time.Second)
	if err == nil {
		t.Fatal("Run returned nil for an invalid address")
	}
}
