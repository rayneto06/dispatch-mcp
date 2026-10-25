package dispatch

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/rayneto06/dispatch-mcp/internal/geo"
	"github.com/rayneto06/dispatch-mcp/internal/health"
	"github.com/rayneto06/dispatch-mcp/internal/partner"
	"github.com/rayneto06/dispatch-mcp/internal/routing"
)

// Partner is what the service needs from a fleet partner. *partner.Client
// satisfies it without saying so; tests use dispatchtest.Partner.
type Partner interface {
	Name() string
	Quote(ctx context.Context, req partner.QuoteRequest) (partner.Quote, error)
	CreateDelivery(ctx context.Context, idempotencyKey string, req partner.CreateRequest) (partner.Delivery, error)
	GetDelivery(ctx context.Context, id string) (partner.Delivery, error)
}

type Router interface {
	Route(ctx context.Context, from, to geo.Point) (routing.Route, error)
}

var (
	ErrInvalidLocation = errors.New("invalid location")
	ErrOutOfArea       = errors.New("pickup and dropoff are too far apart")
	ErrSamePoint       = errors.New("pickup and dropoff are the same point")
	ErrNoQuotes        = errors.New("no partner returned a quote")
)

type Config struct {
	RouteBudget   time.Duration // time allowed for OSRM before falling back
	QuoteDeadline time.Duration // time allowed for the partner quote round
	CreateTimeout time.Duration // time allowed for booking, retries included
	PollAfter     time.Duration // refresh from the partner when older than this
	PollBudget    time.Duration // time allowed for that refresh
	QuoteTTL      time.Duration // upper bound on a quote's validity
	MaxDistanceKm float64       // straight-line limit of the service area
}

func DefaultConfig() Config {
	return Config{
		RouteBudget:   time.Second,
		QuoteDeadline: 1500 * time.Millisecond,
		CreateTimeout: 8 * time.Second,
		PollAfter:     30 * time.Second,
		PollBudget:    3 * time.Second,
		QuoteTTL:      10 * time.Minute,
		MaxDistanceKm: 50,
	}
}

type Service struct {
	cfg      Config
	store    *Store
	router   Router
	partners []Partner
	byName   map[string]Partner
	health   *health.Registry
	now      func() time.Time
}

func NewService(cfg Config, store *Store, router Router, partners []Partner, reg *health.Registry, now func() time.Time) *Service {
	byName := make(map[string]Partner, len(partners))
	for _, p := range partners {
		byName[p.Name()] = p
	}
	return &Service{cfg: cfg, store: store, router: router, partners: partners, byName: byName, health: reg, now: now}
}

// QuoteAll routes the trip once, then asks every partner for a price at the
// same time and waits at most QuoteDeadline. Partners that are slow or fail
// are reported, not fatal; only zero quotes is an error.
func (s *Service) QuoteAll(ctx context.Context, pickup, dropoff geo.Point) (QuoteResult, error) {
	if err := pickup.Validate(); err != nil {
		return QuoteResult{}, fmt.Errorf("%w: pickup: %v", ErrInvalidLocation, err)
	}
	if err := dropoff.Validate(); err != nil {
		return QuoteResult{}, fmt.Errorf("%w: dropoff: %v", ErrInvalidLocation, err)
	}
	if pickup == dropoff {
		return QuoteResult{}, ErrSamePoint
	}
	if geo.DistanceKm(pickup, dropoff) > s.cfg.MaxDistanceKm {
		return QuoteResult{}, ErrOutOfArea
	}

	route := s.route(ctx, pickup, dropoff)

	ctx, cancel := context.WithTimeout(ctx, s.cfg.QuoteDeadline)
	defer cancel() // tells partners still working to stop
	type result struct {
		partner string
		quote   partner.Quote
		err     error
	}
	// Buffered to one slot per partner: a partner that answers after we
	// stopped listening can still send, so its goroutine always exits.
	results := make(chan result, len(s.partners))
	req := partner.QuoteRequest{Pickup: pickup, Dropoff: dropoff, DistanceM: route.DistanceM}
	for _, p := range s.partners {
		go func() {
			q, err := p.Quote(ctx, req)
			results <- result{p.Name(), q, err}
		}()
	}

	now := s.now()
	out := QuoteResult{
		Route: RouteInfo{
			DistanceKm:  math.Round(route.DistanceM/100) / 10,
			DurationMin: ceilMinutes(route.Duration),
			Source:      route.Source,
		},
		Quotes:      []Quote{},
		Unavailable: []Unavailable{},
	}
	answered := map[string]bool{}
	allWaking := true // every failure looks like a partner that is not up yet
collect:
	for range s.partners {
		select {
		case r := <-results:
			answered[r.partner] = true
			if r.err != nil {
				out.Unavailable = append(out.Unavailable, Unavailable{r.partner, reason(r.partner, r.err, s.cfg.QuoteDeadline)})
				allWaking = allWaking && wakingUp(r.err)
				continue
			}
			out.Quotes = append(out.Quotes, s.newQuote(r.partner, r.quote, route, pickup, dropoff, now))
		case <-ctx.Done():
			break collect
		}
	}
	for _, p := range s.partners {
		if !answered[p.Name()] {
			out.Unavailable = append(out.Unavailable, Unavailable{p.Name(), timedOut(s.cfg.QuoteDeadline)})
		}
	}
	slices.SortStableFunc(out.Quotes, func(a, b Quote) int {
		return cmp.Or(cmp.Compare(a.PriceCents, b.PriceCents), cmp.Compare(a.DropoffETAMin, b.DropoffETAMin))
	})
	slices.SortFunc(out.Unavailable, func(a, b Unavailable) int { return cmp.Compare(a.Partner, b.Partner) })

	if len(out.Quotes) == 0 {
		reasons := make([]string, len(out.Unavailable))
		for i, u := range out.Unavailable {
			reasons[i] = u.Partner + ": " + u.Reason
		}
		msg := strings.Join(reasons, "; ")
		if allWaking {
			msg += " (the demo's partners may be waking up from idle; retry in a few seconds)"
		}
		return QuoteResult{}, fmt.Errorf("%w: %s", ErrNoQuotes, msg)
	}
	if err := s.store.PutQuotes(out.Quotes, now); err != nil {
		return QuoteResult{}, err
	}
	return out, nil
}

// route asks OSRM within its own budget and falls back to an estimate, so a
// routing outage degrades quotes instead of failing them.
func (s *Service) route(ctx context.Context, from, to geo.Point) routing.Route {
	ctx, cancel := context.WithTimeout(ctx, s.cfg.RouteBudget)
	defer cancel()
	s.health.Routing.OSRMCalls.Add(1)
	r, err := s.router.Route(ctx, from, to)
	if err != nil {
		s.health.Routing.OSRMFailures.Add(1)
		s.health.Routing.FallbackEstimates.Add(1)
		return routing.Estimate(from, to)
	}
	return r
}

func (s *Service) newQuote(name string, q partner.Quote, route routing.Route, pickup, dropoff geo.Point, now time.Time) Quote {
	validFor := s.cfg.QuoteTTL
	if q.ValidFor > 0 {
		validFor = min(q.ValidFor, s.cfg.QuoteTTL)
	}
	pickupMin := ceilMinutes(q.PickupETA)
	return Quote{
		ID:            newID("q_"),
		Partner:       name,
		PriceCents:    q.PriceCents,
		Currency:      q.Currency,
		PickupETAMin:  pickupMin,
		DropoffETAMin: pickupMin + ceilMinutes(route.Duration),
		ExpiresAt:     now.Add(validFor),
		Pickup:        pickup,
		Dropoff:       dropoff,
	}
}

// Create books a quote with its partner. The quote ID doubles as the
// idempotency key: stable for the logical booking, the same on every retry
// and every repeated call, so concurrent or repeated calls converge on one
// delivery at the partner and in the store.
func (s *Service) Create(ctx context.Context, quoteID string) (Delivery, bool, error) {
	if d, ok := s.store.DeliveryByQuote(quoteID); ok {
		return d, true, nil
	}
	q, err := s.store.Quote(quoteID, s.now())
	if err != nil {
		return Delivery{}, false, err
	}
	p, ok := s.byName[q.Partner]
	if !ok {
		return Delivery{}, false, fmt.Errorf("quote %s names unknown partner %q", quoteID, q.Partner)
	}

	ctx, cancel := context.WithTimeout(ctx, s.cfg.CreateTimeout)
	defer cancel()
	pd, err := p.CreateDelivery(ctx, quoteID, partner.CreateRequest{
		Pickup:          q.Pickup,
		Dropoff:         q.Dropoff,
		PriceCents:      q.PriceCents,
		ClientReference: quoteID,
	})
	if err != nil {
		return Delivery{}, false, err
	}

	now := s.now()
	status, ok := ParseStatus(pd.Status)
	if !ok {
		status = StatusPending // the partner accepted the booking; record it either way
	}
	return s.store.AddDelivery(Delivery{
		ID:                newID("d_"),
		QuoteID:           quoteID,
		Partner:           q.Partner,
		PartnerDeliveryID: pd.ID,
		Status:            status,
		PriceCents:        q.PriceCents,
		Currency:          q.Currency,
		Pickup:            q.Pickup,
		Dropoff:           q.Dropoff,
		CreatedAt:         now,
		UpdatedAt:         now,
		LastUpdateSource:  SourceCreate,
		History:           []HistoryEntry{{Status: status, At: now, Source: SourceCreate}},
		Sequence:          pd.Sequence,
		SyncedAt:          now,
	})
}

// Get returns a delivery. Webhooks normally keep it current; if we haven't
// heard from the partner for PollAfter, it asks the partner directly, so a
// lost webhook only delays an update. A failed refresh is a note, not an
// error: the local state is still the best answer we have.
func (s *Service) Get(ctx context.Context, id string) (Delivery, string, error) {
	d, err := s.store.Delivery(id)
	if err != nil {
		return Delivery{}, "", err
	}
	if d.Status.Terminal() || s.now().Sub(d.SyncedAt) <= s.cfg.PollAfter {
		return d, "", nil
	}
	p, ok := s.byName[d.Partner]
	if !ok {
		return d, "", nil
	}

	ctx, cancel := context.WithTimeout(ctx, s.cfg.PollBudget)
	defer cancel()
	pd, err := p.GetDelivery(ctx, d.PartnerDeliveryID)
	switch {
	case errors.Is(err, partner.ErrNotFound):
		return d, fmt.Sprintf("partner %s no longer knows this delivery (the demo partner restarted)", d.Partner), nil
	case err != nil:
		return d, fmt.Sprintf("could not refresh from partner %s: %s", d.Partner, reason(d.Partner, err, s.cfg.PollBudget)), nil
	}

	status, ok := ParseStatus(pd.Status)
	if !ok {
		return d, fmt.Sprintf("partner %s reported an unknown status %q", d.Partner, pd.Status), nil
	}
	at := pd.UpdatedAt
	if at.IsZero() {
		at = s.now()
	}
	s.store.ApplyStatus(StatusUpdate{
		Partner:           d.Partner,
		PartnerDeliveryID: d.PartnerDeliveryID,
		Status:            status,
		Sequence:          pd.Sequence,
		At:                at,
		Source:            SourcePoll,
	}, s.now())
	d, err = s.store.Delivery(id)
	return d, "", err
}

func (s *Service) List(cursor string, limit int) ([]Delivery, string, error) {
	switch {
	case limit <= 0:
		limit = 10
	case limit > 50:
		limit = 50
	}
	return s.store.List(cursor, limit)
}

// reason turns a partner error into the short text an agent reads. budget
// is the deadline the call ran under, reported when it ran out.
func reason(name string, err error, budget time.Duration) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return timedOut(budget)
	}
	return strings.TrimPrefix(err.Error(), "partner "+name+": ")
}

func timedOut(budget time.Duration) string {
	return "timed out after " + budget.String()
}

// wakingUp reports failures where the partner never answered at all, which
// is what a scaled-to-zero partner looks like while it starts.
func wakingUp(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var apiErr *partner.APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == 0
}

func ceilMinutes(d time.Duration) int {
	return int(math.Ceil(d.Minutes()))
}

func newID(prefix string) string {
	b := make([]byte, 8)
	rand.Read(b) // crypto/rand.Read never returns an error
	return prefix + hex.EncodeToString(b)
}
