package partner

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// waitIdle blocks until no ping is in flight, so the next Kick is decided
// synchronously and the test needs no sleeps.
func waitIdle(t *testing.T, w *Warmer) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		w.mu.Lock()
		busy := w.inflight
		w.mu.Unlock()
		if !busy {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("ping still in flight after 2s")
}

func TestWarmerPingsOncePerIdleWindow(t *testing.T) {
	var calls atomic.Int32
	pinged := make(chan struct{}, 10)
	var mu sync.Mutex
	clock := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	now := func() time.Time { mu.Lock(); defer mu.Unlock(); return clock }
	advance := func(d time.Duration) { mu.Lock(); clock = clock.Add(d); mu.Unlock() }

	w := NewWarmer(func(context.Context) error {
		calls.Add(1)
		pinged <- struct{}{}
		return nil
	}, 10*time.Minute, now, slog.New(slog.DiscardHandler))

	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() { defer wg.Done(); w.Kick() }()
	}
	wg.Wait()
	<-pinged
	waitIdle(t, w)
	if n := calls.Load(); n != 1 {
		t.Fatalf("pings after 10 concurrent kicks = %d, want 1", n)
	}

	advance(time.Minute)
	w.Kick() // within the idle window: returns without starting a ping
	waitIdle(t, w)
	if n := calls.Load(); n != 1 {
		t.Errorf("pings within the idle window = %d, want 1", n)
	}

	advance(10 * time.Minute)
	w.Kick()
	select {
	case <-pinged:
	case <-time.After(time.Second):
		t.Fatal("no ping after the idle window passed")
	}
}

func TestWarmerRetriesAfterFailedPing(t *testing.T) {
	var calls atomic.Int32
	pinged := make(chan struct{}, 10)
	w := NewWarmer(func(context.Context) error {
		calls.Add(1)
		pinged <- struct{}{}
		return context.DeadlineExceeded
	}, 10*time.Minute, time.Now, slog.New(slog.DiscardHandler))

	w.Kick()
	<-pinged
	waitIdle(t, w)
	w.Kick() // the first ping failed, so the partner may still be asleep
	select {
	case <-pinged:
	case <-time.After(time.Second):
		t.Fatal("a failed ping should not suppress the next kick")
	}
}
