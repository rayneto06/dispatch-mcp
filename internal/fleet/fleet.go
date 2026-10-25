// Package fleet simulates delivery fleet partners. Each partner has its own
// REST API under /<name>/v1, its own prices and latency, advances bookings
// through pending → assigned → picked_up → delivered on timers, and reports
// each change with an HMAC-signed webhook.
//
// It exists so dispatch-mcp has something realistic, and misbehaving on
// purpose, to integrate with.
package fleet

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	randv2 "math/rand/v2"
	"net/http"
	"sync"
	"time"
)

// Profile describes how one simulated partner prices and behaves.
type Profile struct {
	Name         string
	BaseCents    int
	PerKmCents   int
	QuoteLatency [2]time.Duration // min, max added to each quote request
	Latency      [2]time.Duration // min, max added to other API requests
	PickupETA    [2]time.Duration // min, max quoted time until pickup

	// Failure injection, each a probability in [0, 1].
	Rate429        float64 // answer 429 with Retry-After: 1, do nothing
	Rate503        float64 // answer 503, do nothing
	CommitThenFail float64 // book the delivery, then answer 503 anyway
	DuplicateRate  float64 // send a webhook event twice
}

// Timings are offsets from creation at which a delivery changes status.
type Timings struct {
	Assigned, PickedUp, Delivered time.Duration
}

// DefaultTimings complete a delivery in about two and a half minutes.
func DefaultTimings() Timings {
	return Timings{Assigned: 10 * time.Second, PickedUp: 60 * time.Second, Delivered: 150 * time.Second}
}

// DefaultProfiles are the three demo partners: alpha is reliable, beta is
// slow to quote, gamma is cheap but flaky. All of them deliver webhooks at
// least once, so some events arrive twice.
func DefaultProfiles() []Profile {
	ms := time.Millisecond
	return []Profile{
		{Name: "alpha", BaseCents: 699, PerKmCents: 150,
			QuoteLatency: [2]time.Duration{50 * ms, 200 * ms}, Latency: [2]time.Duration{50 * ms, 200 * ms},
			PickupETA: [2]time.Duration{8 * time.Minute, 15 * time.Minute}, DuplicateRate: 0.10},
		{Name: "beta", BaseCents: 899, PerKmCents: 170,
			QuoteLatency: [2]time.Duration{800 * ms, 2500 * ms}, Latency: [2]time.Duration{100 * ms, 300 * ms},
			PickupETA: [2]time.Duration{5 * time.Minute, 10 * time.Minute}, DuplicateRate: 0.10},
		{Name: "gamma", BaseCents: 499, PerKmCents: 120,
			QuoteLatency: [2]time.Duration{50 * ms, 300 * ms}, Latency: [2]time.Duration{50 * ms, 300 * ms},
			PickupETA: [2]time.Duration{10 * time.Minute, 20 * time.Minute},
			Rate429:   0.15, Rate503: 0.15, CommitThenFail: 0.10, DuplicateRate: 0.30},
	}
}

// Config wires a Fleet to its secrets and to the webhook receiver.
type Config struct {
	APIKey        string
	WebhookSecret []byte
	WebhookURL    string // events go to WebhookURL + "/webhooks/" + partner
	Timings       Timings
	Jitter        float64        // ± fraction applied to each timing; 0 disables
	Rand          func() float64 // [0,1); nil uses math/rand/v2
	HTTPClient    *http.Client   // for webhooks; nil uses a 5 s timeout
	Logger        *slog.Logger
}

// Fleet is the set of simulated partners plus their webhook sender.
type Fleet struct {
	cfg      Config
	partners map[string]*partnerState

	mu     sync.Mutex // guards partners' maps and closed
	closed bool

	queue      chan event
	done       chan struct{}  // closed by Shutdown: stops progressions
	progress   sync.WaitGroup // one per delivery still progressing
	workers    sync.WaitGroup
	sendCtx    context.Context // cancelled if Shutdown runs out of time
	cancelSend context.CancelFunc
	shutdown   sync.Once
}

type partnerState struct {
	profile    Profile
	deliveries map[string]*deliveryResponse
	byKey      map[string]idempotentEntry // Idempotency-Key → first request and its response
}

type idempotentEntry struct {
	request createRequest
	resp    deliveryResponse
}

const webhookWorkers = 2

// New builds a Fleet. Call Start to begin sending webhooks.
func New(cfg Config, profiles []Profile) *Fleet {
	if cfg.Rand == nil {
		cfg.Rand = randv2.Float64
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 5 * time.Second}
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	f := &Fleet{
		cfg:      cfg,
		partners: make(map[string]*partnerState, len(profiles)),
		queue:    make(chan event, 256),
		done:     make(chan struct{}),
	}
	// Background sends outlive any single request, so they get their own
	// context, cancelled only when shutdown runs out of time.
	f.sendCtx, f.cancelSend = context.WithCancel(context.Background())
	for _, p := range profiles {
		f.partners[p.Name] = &partnerState{
			profile:    p,
			deliveries: map[string]*deliveryResponse{},
			byKey:      map[string]idempotentEntry{},
		}
	}
	return f
}

// Start launches the webhook workers.
func (f *Fleet) Start() {
	for range webhookWorkers {
		f.workers.Add(1)
		go f.worker()
	}
}

// Shutdown stops new bookings and status changes, then waits for queued
// webhooks to be delivered until ctx is done. Safe to call more than once.
func (f *Fleet) Shutdown(ctx context.Context) error {
	var err error
	f.shutdown.Do(func() {
		f.mu.Lock()
		f.closed = true // no new progress goroutines after this
		f.mu.Unlock()
		close(f.done)
		f.progress.Wait()
		close(f.queue) // nothing can enqueue any more; workers drain and exit

		drained := make(chan struct{})
		go func() {
			f.workers.Wait()
			close(drained)
		}()
		select {
		case <-drained:
		case <-ctx.Done():
			f.cancelSend()
			<-drained
			err = ctx.Err()
		}
		f.cancelSend()
	})
	return err
}

// between returns a random duration in [r[0], r[1]].
func (f *Fleet) between(r [2]time.Duration) time.Duration {
	if r[1] <= r[0] {
		return r[0]
	}
	return r[0] + time.Duration(f.cfg.Rand()*float64(r[1]-r[0]))
}

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}
