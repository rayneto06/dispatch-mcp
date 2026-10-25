// Package dispatch is the core of dispatch-mcp: quotes, deliveries and the
// rules that keep them consistent while partners fail, retry and send
// webhooks out of order.
package dispatch

import (
	"errors"
	"time"

	"github.com/rayneto06/dispatch-mcp/internal/geo"
)

type Status string

const (
	StatusPending   Status = "pending"
	StatusAssigned  Status = "assigned"
	StatusPickedUp  Status = "picked_up"
	StatusDelivered Status = "delivered"
)

// Terminal reports whether the status can no longer change.
func (s Status) Terminal() bool { return s == StatusDelivered }

// Source says how we learned about a status.
type Source string

const (
	SourceCreate  Source = "create"
	SourceWebhook Source = "webhook"
	SourcePoll    Source = "poll"
)

type RouteInfo struct {
	DistanceKm  float64 `json:"distance_km"`
	DurationMin int     `json:"duration_min" jsonschema:"driving time from pickup to dropoff, in minutes"`
	Source      string  `json:"source" jsonschema:"osrm when routed by OSRM, estimate when OSRM was unavailable"`
}

type Quote struct {
	ID            string    `json:"quote_id" jsonschema:"pass this to create_delivery to book"`
	Partner       string    `json:"partner"`
	PriceCents    int       `json:"price_cents"`
	Currency      string    `json:"currency"`
	PickupETAMin  int       `json:"pickup_eta_min" jsonschema:"minutes until the courier picks up"`
	DropoffETAMin int       `json:"dropoff_eta_min" jsonschema:"minutes until delivery, counted from now"`
	ExpiresAt     time.Time `json:"expires_at"`
	Pickup        geo.Point `json:"-"`
	Dropoff       geo.Point `json:"-"`
}

type Unavailable struct {
	Partner string `json:"partner"`
	Reason  string `json:"reason"`
}

type QuoteResult struct {
	Route       RouteInfo     `json:"route"`
	Quotes      []Quote       `json:"quotes" jsonschema:"cheapest first"`
	Unavailable []Unavailable `json:"unavailable" jsonschema:"partners that did not return a quote, and why"`
}

type HistoryEntry struct {
	Status Status    `json:"status"`
	At     time.Time `json:"at"`
	Source Source    `json:"source" jsonschema:"create, webhook (pushed by the partner) or poll (fetched by us)"`
}

type Delivery struct {
	ID                string         `json:"delivery_id"`
	QuoteID           string         `json:"quote_id"`
	Partner           string         `json:"partner"`
	PartnerDeliveryID string         `json:"partner_delivery_id"`
	Status            Status         `json:"status" jsonschema:"pending, assigned, picked_up or delivered"`
	PriceCents        int            `json:"price_cents"`
	Currency          string         `json:"currency"`
	Pickup            geo.Point      `json:"pickup"`
	Dropoff           geo.Point      `json:"dropoff"`
	CreatedAt         time.Time      `json:"created_at"`
	UpdatedAt         time.Time      `json:"updated_at"`
	LastUpdateSource  Source         `json:"last_update_source"`
	History           []HistoryEntry `json:"history,omitempty"`

	Sequence int64     `json:"-"` // partner's version of the delivery; only newer updates apply
	SyncedAt time.Time `json:"-"` // last time the partner told us anything about it
}

// clone returns a copy that shares no memory with d.
func (d Delivery) clone() Delivery {
	d.History = append([]HistoryEntry(nil), d.History...)
	return d
}

// StatusUpdate is a status reported by a partner, by webhook or by poll.
type StatusUpdate struct {
	Partner           string
	PartnerDeliveryID string
	Status            Status
	Sequence          int64
	At                time.Time
	Source            Source
}

type ApplyResult int

const (
	Applied ApplyResult = iota
	Stale               // not newer than what we have
	Unknown             // no such delivery
)

var (
	ErrQuoteNotFound    = errors.New("quote not found")
	ErrQuoteExpired     = errors.New("quote expired")
	ErrDeliveryNotFound = errors.New("delivery not found")
	ErrInvalidCursor    = errors.New("invalid cursor")
	ErrCapacity         = errors.New("capacity reached")
)
