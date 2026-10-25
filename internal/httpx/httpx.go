// Package httpx holds the HTTP plumbing both binaries share: a server with
// sane timeouts, a Run loop with graceful shutdown, and two middlewares.
package httpx

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"time"

	"golang.org/x/time/rate"
)

// RequireToken serves next only when the {token} path value equals token.
// Anything else gets a plain 404, so the endpoint does not reveal it exists.
// The comparison is constant time so response timing does not leak the token.
func RequireToken(token string, next http.Handler) http.Handler {
	want := []byte(token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := []byte(r.PathValue("token"))
		if len(want) == 0 || subtle.ConstantTimeCompare(got, want) != 1 {
			http.NotFound(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// RateLimit rejects requests with 429 when the limiter has no token left.
func RateLimit(l *rate.Limiter, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !l.Allow() {
			w.Header().Set("Retry-After", "1")
			http.Error(w, "rate limited", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// NewServer returns an http.Server with explicit timeouts. The zero values
// mean "no timeout", which lets slow clients hold connections forever.
func NewServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
}

// Run serves until ctx is cancelled, then shuts the server down, giving
// in-flight requests up to shutdownTimeout to finish.
func Run(ctx context.Context, srv *http.Server, shutdownTimeout time.Duration) error {
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()

	select {
	case err := <-errc:
		return err // failed to start (bad address, port in use)
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return err
	}
	if err := <-errc; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
