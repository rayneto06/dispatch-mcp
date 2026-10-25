// Package dispatchtest has in-memory partners and routers for tests of the
// dispatch service and the MCP tools.
package dispatchtest

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/rayneto06/dispatch-mcp/internal/geo"
	"github.com/rayneto06/dispatch-mcp/internal/partner"
	"github.com/rayneto06/dispatch-mcp/internal/routing"
)

// Partner implements dispatch.Partner. Each nil Fn gets a working default:
// a fixed quote, an idempotent booking keyed by idempotency key, and a
// lookup of booked deliveries.
type Partner struct {
	PartnerName string
	QuoteFn     func(ctx context.Context, req partner.QuoteRequest) (partner.Quote, error)
	CreateFn    func(ctx context.Context, key string, req partner.CreateRequest) (partner.Delivery, error)
	GetFn       func(ctx context.Context, id string) (partner.Delivery, error)
	CreateDelay time.Duration // widens race windows in concurrency tests

	mu       sync.Mutex
	byKey    map[string]partner.Delivery
	bookings int
}

func (p *Partner) Name() string { return p.PartnerName }

func (p *Partner) Quote(ctx context.Context, req partner.QuoteRequest) (partner.Quote, error) {
	if p.QuoteFn != nil {
		return p.QuoteFn(ctx, req)
	}
	return partner.Quote{PriceCents: 1000, Currency: "USD", PickupETA: 10 * time.Minute, ValidFor: 10 * time.Minute}, nil
}

func (p *Partner) CreateDelivery(ctx context.Context, key string, req partner.CreateRequest) (partner.Delivery, error) {
	if p.CreateFn != nil {
		return p.CreateFn(ctx, key, req)
	}
	time.Sleep(p.CreateDelay)
	p.mu.Lock()
	defer p.mu.Unlock()
	if d, ok := p.byKey[key]; ok {
		return d, nil
	}
	if p.byKey == nil {
		p.byKey = map[string]partner.Delivery{}
	}
	p.bookings++
	now := time.Now().UTC()
	d := partner.Delivery{ID: fmt.Sprintf("%s_%d", p.PartnerName, p.bookings), Status: "pending", Sequence: 1, CreatedAt: now, UpdatedAt: now}
	p.byKey[key] = d
	return d, nil
}

func (p *Partner) GetDelivery(ctx context.Context, id string) (partner.Delivery, error) {
	if p.GetFn != nil {
		return p.GetFn(ctx, id)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, d := range p.byKey {
		if d.ID == id {
			return d, nil
		}
	}
	return partner.Delivery{}, &partner.APIError{Partner: p.PartnerName, StatusCode: 404, Attempts: 1}
}

// Bookings is how many distinct deliveries the default CreateDelivery made.
func (p *Partner) Bookings() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.bookings
}

// Router implements dispatch.Router. A nil RouteFn returns a 4 km, 10 minute
// OSRM route.
type Router struct {
	RouteFn func(ctx context.Context, from, to geo.Point) (routing.Route, error)
}

func (r Router) Route(ctx context.Context, from, to geo.Point) (routing.Route, error) {
	if r.RouteFn != nil {
		return r.RouteFn(ctx, from, to)
	}
	return routing.Route{DistanceM: 4000, Duration: 10 * time.Minute, Source: routing.SourceOSRM}, nil
}
