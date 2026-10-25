// Package health counts what happens at the integration edges (partner
// calls, webhooks, routing) so the partner_health tool can report it.
// Counters are in-process and reset on restart.
package health

import (
	"sync"
	"sync/atomic"
	"time"
)

// Counters for one partner. The atomic fields are safe to bump from any
// goroutine; the last error needs a mutex because it is two values.
type Counters struct {
	QuoteRequests  atomic.Int64
	QuotesReturned atomic.Int64
	CreateRequests atomic.Int64
	Attempts       atomic.Int64 // HTTP attempts, including retries
	Retries        atomic.Int64
	RateLimited    atomic.Int64 // 429 responses
	ServerErrors   atomic.Int64 // 5xx responses
	Timeouts       atomic.Int64
	Polls          atomic.Int64 // status refreshes by get_delivery

	WebhooksReceived  atomic.Int64
	DuplicatesDropped atomic.Int64
	OutOfOrderDropped atomic.Int64
	UnknownDelivery   atomic.Int64
	BadSignatures     atomic.Int64

	mu        sync.Mutex
	lastErr   string
	lastErrAt time.Time
}

// RecordError remembers the most recent failure.
func (c *Counters) RecordError(msg string, at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastErr, c.lastErrAt = msg, at
}

// Routing counts OSRM usage.
type Routing struct {
	OSRMCalls         atomic.Int64
	OSRMFailures      atomic.Int64
	FallbackEstimates atomic.Int64
}

// Registry holds the counters of every partner. The partner set is fixed at
// construction, so the map is only read afterwards and needs no lock.
type Registry struct {
	started  time.Time
	names    []string
	partners map[string]*Counters
	Routing  Routing
}

func NewRegistry(partners []string, started time.Time) *Registry {
	r := &Registry{started: started, names: partners, partners: make(map[string]*Counters, len(partners))}
	for _, p := range partners {
		r.partners[p] = &Counters{}
	}
	return r
}

// Partner returns the counters for name, or nil for an unknown partner.
func (r *Registry) Partner(name string) *Counters {
	return r.partners[name]
}

// Snapshot is the JSON shape returned by the partner_health tool.
type Snapshot struct {
	Since    time.Time         `json:"since" jsonschema:"when the counters started (process start)"`
	Partners []PartnerSnapshot `json:"partners"`
	Routing  RoutingSnapshot   `json:"routing"`
}

type PartnerSnapshot struct {
	Partner           string     `json:"partner"`
	QuoteRequests     int64      `json:"quote_requests"`
	QuotesReturned    int64      `json:"quotes_returned"`
	CreateRequests    int64      `json:"create_requests"`
	Attempts          int64      `json:"http_attempts" jsonschema:"HTTP requests sent to the partner, retries included"`
	Retries           int64      `json:"retries"`
	RateLimited       int64      `json:"rate_limited_429"`
	ServerErrors      int64      `json:"server_errors_5xx"`
	Timeouts          int64      `json:"timeouts"`
	Polls             int64      `json:"status_polls"`
	WebhooksReceived  int64      `json:"webhooks_received"`
	DuplicatesDropped int64      `json:"duplicates_dropped" jsonschema:"webhooks ignored because the event ID was already processed"`
	OutOfOrderDropped int64      `json:"out_of_order_dropped" jsonschema:"webhooks ignored because a newer status was already applied"`
	UnknownDelivery   int64      `json:"unknown_delivery_events" jsonschema:"webhooks for a delivery not known yet; held for a few minutes in case its booking response is still on the way"`
	BadSignatures     int64      `json:"bad_signatures"`
	LastError         string     `json:"last_error,omitempty"`
	LastErrorAt       *time.Time `json:"last_error_at,omitempty"`
}

type RoutingSnapshot struct {
	OSRMCalls         int64 `json:"osrm_calls"`
	OSRMFailures      int64 `json:"osrm_failures"`
	FallbackEstimates int64 `json:"fallback_estimates" jsonschema:"routes estimated from straight-line distance because OSRM was unavailable"`
}

func (r *Registry) Snapshot() Snapshot {
	s := Snapshot{
		Since:    r.started,
		Partners: make([]PartnerSnapshot, 0, len(r.names)),
		Routing: RoutingSnapshot{
			OSRMCalls:         r.Routing.OSRMCalls.Load(),
			OSRMFailures:      r.Routing.OSRMFailures.Load(),
			FallbackEstimates: r.Routing.FallbackEstimates.Load(),
		},
	}
	for _, name := range r.names {
		c := r.partners[name]
		ps := PartnerSnapshot{
			Partner:           name,
			QuoteRequests:     c.QuoteRequests.Load(),
			QuotesReturned:    c.QuotesReturned.Load(),
			CreateRequests:    c.CreateRequests.Load(),
			Attempts:          c.Attempts.Load(),
			Retries:           c.Retries.Load(),
			RateLimited:       c.RateLimited.Load(),
			ServerErrors:      c.ServerErrors.Load(),
			Timeouts:          c.Timeouts.Load(),
			Polls:             c.Polls.Load(),
			WebhooksReceived:  c.WebhooksReceived.Load(),
			DuplicatesDropped: c.DuplicatesDropped.Load(),
			OutOfOrderDropped: c.OutOfOrderDropped.Load(),
			UnknownDelivery:   c.UnknownDelivery.Load(),
			BadSignatures:     c.BadSignatures.Load(),
		}
		c.mu.Lock()
		if c.lastErr != "" {
			at := c.lastErrAt
			ps.LastError, ps.LastErrorAt = c.lastErr, &at
		}
		c.mu.Unlock()
		s.Partners = append(s.Partners, ps)
	}
	return s
}
