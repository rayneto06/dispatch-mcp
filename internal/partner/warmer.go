package partner

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Warmer wakes a scale-to-zero partner before it is needed. Kick is cheap
// and non-blocking, so it can be called on every incoming MCP request; it
// pings at most once per idle window, and never twice at the same time.
type Warmer struct {
	ping   func(context.Context) error
	idle   time.Duration
	now    func() time.Time
	logger *slog.Logger

	mu       sync.Mutex
	lastOK   time.Time
	inflight bool
}

func NewWarmer(ping func(context.Context) error, idle time.Duration, now func() time.Time, logger *slog.Logger) *Warmer {
	return &Warmer{ping: ping, idle: idle, now: now, logger: logger}
}

func (w *Warmer) Kick() {
	w.mu.Lock()
	recent := !w.lastOK.IsZero() && w.now().Sub(w.lastOK) < w.idle
	if w.inflight || recent {
		w.mu.Unlock()
		return
	}
	w.inflight = true
	w.mu.Unlock()

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		start := time.Now()
		err := w.ping(ctx)

		w.mu.Lock()
		w.inflight = false
		if err == nil {
			w.lastOK = w.now()
		}
		w.mu.Unlock()

		if err != nil {
			w.logger.Warn("partner warm-up failed", "error", err)
			return
		}
		w.logger.Info("partner warm-up", "duration_ms", time.Since(start).Milliseconds())
	}()
}
