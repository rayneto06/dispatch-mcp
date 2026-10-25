package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rayneto06/dispatch-mcp/internal/dispatch/dispatchtest"
	"github.com/rayneto06/dispatch-mcp/internal/geo"
	"github.com/rayneto06/dispatch-mcp/internal/health"
	"github.com/rayneto06/dispatch-mcp/internal/partner"
	"github.com/rayneto06/dispatch-mcp/internal/routing"
)

var (
	ferryBuilding  = geo.Point{Lat: 37.7955, Lon: -122.3937}
	missionDolores = geo.Point{Lat: 37.7599, Lon: -122.4268}
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) set(t time.Time)     { c.mu.Lock(); c.t = t; c.mu.Unlock() }
func (c *clock) add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

type env struct {
	svc   *Service
	store *Store
	reg   *health.Registry
	clock *clock
}

func newEnv(t *testing.T, cfg Config, router Router, partners ...*dispatchtest.Partner) *env {
	t.Helper()
	names := make([]string, len(partners))
	ps := make([]Partner, len(partners))
	for i, p := range partners {
		names[i], ps[i] = p.PartnerName, p
	}
	e := &env{store: NewStore(DefaultLimits()), reg: health.NewRegistry(names, t0), clock: &clock{t: t0}}
	e.svc = NewService(cfg, e.store, router, ps, e.reg, e.clock.now)
	return e
}

func priced(name string, cents int) *dispatchtest.Partner {
	return &dispatchtest.Partner{PartnerName: name, QuoteFn: func(context.Context, partner.QuoteRequest) (partner.Quote, error) {
		return partner.Quote{PriceCents: cents, Currency: "USD", PickupETA: 10 * time.Minute, ValidFor: 10 * time.Minute}, nil
	}}
}

func failing(name string, err error) *dispatchtest.Partner {
	return &dispatchtest.Partner{PartnerName: name, QuoteFn: func(context.Context, partner.QuoteRequest) (partner.Quote, error) {
		return partner.Quote{}, err
	}}
}

func TestQuoteAllSortsAndReportsUnavailable(t *testing.T) {
	e := newEnv(t, DefaultConfig(), dispatchtest.Router{},
		priced("alpha", 2199),
		failing("beta", &partner.APIError{Partner: "beta", StatusCode: 503, Attempts: 3}),
		priced("gamma", 1699))

	res, err := e.svc.QuoteAll(context.Background(), ferryBuilding, missionDolores)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Quotes) != 2 || res.Quotes[0].Partner != "gamma" || res.Quotes[1].Partner != "alpha" {
		t.Errorf("quotes = %+v, want gamma then alpha", res.Quotes)
	}
	if len(res.Unavailable) != 1 || res.Unavailable[0] != (Unavailable{"beta", "unavailable (503) after 3 attempts"}) {
		t.Errorf("unavailable = %+v", res.Unavailable)
	}
	if !strings.HasPrefix(res.Quotes[0].ID, "q_") || res.Quotes[0].ID == res.Quotes[1].ID {
		t.Errorf("quote IDs = %q, %q", res.Quotes[0].ID, res.Quotes[1].ID)
	}
}

func TestQuoteAllETAsAndStorage(t *testing.T) {
	e := newEnv(t, DefaultConfig(), dispatchtest.Router{}, priced("alpha", 2199))
	res, err := e.svc.QuoteAll(context.Background(), ferryBuilding, missionDolores)
	if err != nil {
		t.Fatal(err)
	}
	q := res.Quotes[0]
	if q.PickupETAMin != 10 || q.DropoffETAMin != 20 || !q.ExpiresAt.Equal(t0.Add(10*time.Minute)) {
		t.Errorf("quote = %+v, want pickup 10, dropoff 20, expiry t0+10m", q)
	}
	if res.Route != (RouteInfo{DistanceKm: 4, DurationMin: 10, Source: routing.SourceOSRM}) {
		t.Errorf("route = %+v", res.Route)
	}
	if _, err := e.store.Quote(q.ID, t0); err != nil {
		t.Errorf("quote not stored: %v", err)
	}
}

func TestQuoteAllDeadline(t *testing.T) {
	cfg := DefaultConfig()
	cfg.QuoteDeadline = 100 * time.Millisecond
	slow := &dispatchtest.Partner{PartnerName: "beta", QuoteFn: func(context.Context, partner.QuoteRequest) (partner.Quote, error) {
		time.Sleep(2 * time.Second) // ignores ctx on purpose
		return partner.Quote{PriceCents: 1}, nil
	}}
	e := newEnv(t, cfg, dispatchtest.Router{}, priced("alpha", 2199), slow)

	start := time.Now()
	res, err := e.svc.QuoteAll(context.Background(), ferryBuilding, missionDolores)
	if elapsed := time.Since(start); elapsed > 150*time.Millisecond {
		t.Errorf("QuoteAll took %v, want about the 100ms deadline", elapsed)
	}
	if err != nil || len(res.Quotes) != 1 {
		t.Fatalf("res = %+v, err = %v", res, err)
	}
	if res.Unavailable[0] != (Unavailable{"beta", "timed out after 100ms"}) {
		t.Errorf("unavailable = %+v", res.Unavailable)
	}
}

func TestQuoteAllDeadlineErrorFromPartner(t *testing.T) {
	// A well-behaved client notices the deadline itself; the reason must read the same.
	cfg := DefaultConfig()
	cfg.QuoteDeadline = 50 * time.Millisecond
	ctxAware := &dispatchtest.Partner{PartnerName: "beta", QuoteFn: func(ctx context.Context, _ partner.QuoteRequest) (partner.Quote, error) {
		<-ctx.Done()
		return partner.Quote{}, &partner.APIError{Partner: "beta", Attempts: 1, Err: ctx.Err()}
	}}
	e := newEnv(t, cfg, dispatchtest.Router{}, priced("alpha", 2199), ctxAware)
	res, _ := e.svc.QuoteAll(context.Background(), ferryBuilding, missionDolores)
	if len(res.Unavailable) != 1 || res.Unavailable[0].Reason != "timed out after 50ms" {
		t.Errorf("unavailable = %+v", res.Unavailable)
	}
}

func TestQuoteAllRouteFallback(t *testing.T) {
	router := dispatchtest.Router{RouteFn: func(context.Context, geo.Point, geo.Point) (routing.Route, error) {
		return routing.Route{}, errors.New("osrm down")
	}}
	e := newEnv(t, DefaultConfig(), router, priced("alpha", 2199))
	res, err := e.svc.QuoteAll(context.Background(), ferryBuilding, missionDolores)
	if err != nil {
		t.Fatal(err)
	}
	if res.Route.Source != routing.SourceEstimate {
		t.Errorf("route source = %q, want estimate", res.Route.Source)
	}
	r := &e.reg.Routing
	if r.OSRMCalls.Load() != 1 || r.OSRMFailures.Load() != 1 || r.FallbackEstimates.Load() != 1 {
		t.Errorf("routing counters = %d/%d/%d", r.OSRMCalls.Load(), r.OSRMFailures.Load(), r.FallbackEstimates.Load())
	}
}

func TestQuoteAllValidation(t *testing.T) {
	e := newEnv(t, DefaultConfig(), dispatchtest.Router{}, priced("alpha", 2199))
	tests := []struct {
		name             string
		pickup, dropoff  geo.Point
		wantIs           error
		wantTextContains string
	}{
		{"swapped pickup", geo.Point{Lat: -122.4, Lon: 37.7}, missionDolores, nil, "pickup: lat must be"},
		{"bad dropoff", ferryBuilding, geo.Point{Lat: 0, Lon: 200}, nil, "dropoff: lon must be"},
		{"same point", ferryBuilding, ferryBuilding, ErrSamePoint, ""},
		{"60 km apart", ferryBuilding, geo.Point{Lat: 37.3382, Lon: -121.8863}, ErrOutOfArea, ""}, // San Jose
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := e.svc.QuoteAll(context.Background(), tt.pickup, tt.dropoff)
			if tt.wantIs != nil && !errors.Is(err, tt.wantIs) {
				t.Errorf("err = %v, want %v", err, tt.wantIs)
			}
			if tt.wantTextContains != "" && (err == nil || !strings.Contains(err.Error(), tt.wantTextContains)) {
				t.Errorf("err = %v, want text %q", err, tt.wantTextContains)
			}
		})
	}
}

func TestQuoteAllNoQuotes(t *testing.T) {
	cfg := DefaultConfig()
	cfg.QuoteDeadline = 50 * time.Millisecond
	asleep := func(name string) *dispatchtest.Partner {
		return failing(name, &partner.APIError{Partner: name, Attempts: 1, Err: errors.New("connection refused")})
	}
	e := newEnv(t, cfg, dispatchtest.Router{}, asleep("alpha"), asleep("beta"))
	_, err := e.svc.QuoteAll(context.Background(), ferryBuilding, missionDolores)
	if !errors.Is(err, ErrNoQuotes) || !strings.Contains(err.Error(), "waking up") || !strings.Contains(err.Error(), "alpha:") {
		t.Errorf("all unreachable: err = %v, want ErrNoQuotes with a waking-up hint", err)
	}

	rejected := failing("alpha", &partner.APIError{Partner: "alpha", StatusCode: 400, Attempts: 1, Body: "bad"})
	e = newEnv(t, cfg, dispatchtest.Router{}, rejected)
	_, err = e.svc.QuoteAll(context.Background(), ferryBuilding, missionDolores)
	if !errors.Is(err, ErrNoQuotes) || strings.Contains(err.Error(), "waking up") {
		t.Errorf("all rejected: err = %v, want ErrNoQuotes without the hint", err)
	}
}

func TestQuoteAllEmptySlicesNotNil(t *testing.T) {
	e := newEnv(t, DefaultConfig(), dispatchtest.Router{}, priced("alpha", 2199))
	res, _ := e.svc.QuoteAll(context.Background(), ferryBuilding, missionDolores)
	b, _ := json.Marshal(res)
	if !strings.Contains(string(b), `"unavailable":[]`) {
		t.Errorf("JSON = %s, want \"unavailable\":[]", b)
	}
}

func quoteOne(t *testing.T, e *env) Quote {
	t.Helper()
	res, err := e.svc.QuoteAll(context.Background(), ferryBuilding, missionDolores)
	if err != nil {
		t.Fatal(err)
	}
	return res.Quotes[0]
}

func TestCreateConcurrentSameQuote(t *testing.T) {
	alpha := &dispatchtest.Partner{PartnerName: "alpha", CreateDelay: 20 * time.Millisecond}
	e := newEnv(t, DefaultConfig(), dispatchtest.Router{}, alpha)
	q := quoteOne(t, e)

	type result struct {
		d       Delivery
		already bool
		err     error
	}
	results := make(chan result, 10)
	for range 10 {
		go func() {
			d, already, err := e.svc.Create(context.Background(), q.ID)
			results <- result{d, already, err}
		}()
	}
	var first string
	fresh := 0
	for range 10 {
		r := <-results
		if r.err != nil {
			t.Fatal(r.err)
		}
		if first == "" {
			first = r.d.ID
		}
		if r.d.ID != first {
			t.Errorf("delivery IDs differ: %s vs %s", r.d.ID, first)
		}
		if !r.already {
			fresh++
		}
	}
	if fresh != 1 || alpha.Bookings() != 1 {
		t.Errorf("fresh results = %d, partner bookings = %d; want 1 and 1", fresh, alpha.Bookings())
	}
	if all, _, _ := e.store.List("", 50); len(all) != 1 {
		t.Errorf("store has %d deliveries, want 1", len(all))
	}
}

func TestCreateExpiredQuote(t *testing.T) {
	called := false
	alpha := &dispatchtest.Partner{PartnerName: "alpha", CreateFn: func(context.Context, string, partner.CreateRequest) (partner.Delivery, error) {
		called = true
		return partner.Delivery{}, nil
	}}
	e := newEnv(t, DefaultConfig(), dispatchtest.Router{}, alpha)
	q := quoteOne(t, e)
	e.clock.add(11 * time.Minute)

	if _, _, err := e.svc.Create(context.Background(), q.ID); !errors.Is(err, ErrQuoteExpired) {
		t.Errorf("err = %v, want ErrQuoteExpired", err)
	}
	if called {
		t.Error("partner was called for an expired quote")
	}
}

func TestCreateRepeatAfterExpiryReturnsExisting(t *testing.T) {
	e := newEnv(t, DefaultConfig(), dispatchtest.Router{}, &dispatchtest.Partner{PartnerName: "alpha"})
	q := quoteOne(t, e)
	d, _, err := e.svc.Create(context.Background(), q.ID)
	if err != nil {
		t.Fatal(err)
	}
	e.clock.add(time.Hour)
	again, already, err := e.svc.Create(context.Background(), q.ID)
	if err != nil || !already || again.ID != d.ID {
		t.Errorf("repeat = %s already=%v err=%v, want %s already=true", again.ID, already, err, d.ID)
	}
}

func TestCreateRecordsDelivery(t *testing.T) {
	e := newEnv(t, DefaultConfig(), dispatchtest.Router{}, &dispatchtest.Partner{PartnerName: "alpha"})
	q := quoteOne(t, e)
	d, already, err := e.svc.Create(context.Background(), q.ID)
	if err != nil || already {
		t.Fatalf("create = %v already=%v", err, already)
	}
	if !strings.HasPrefix(d.ID, "d_") || d.PartnerDeliveryID != "alpha_1" || d.Status != StatusPending ||
		d.PriceCents != q.PriceCents || d.Pickup != ferryBuilding || len(d.History) != 1 || d.History[0].Source != SourceCreate {
		t.Errorf("delivery = %+v", d)
	}
}

func TestGetReconciles(t *testing.T) {
	var polls int
	alpha := &dispatchtest.Partner{PartnerName: "alpha", GetFn: func(_ context.Context, id string) (partner.Delivery, error) {
		polls++
		return partner.Delivery{ID: id, Status: "picked_up", Sequence: 3, UpdatedAt: t0.Add(25 * time.Second)}, nil
	}}
	e := newEnv(t, DefaultConfig(), dispatchtest.Router{}, alpha)
	d, _, _ := e.svc.Create(context.Background(), quoteOne(t, e).ID)

	e.clock.set(t0.Add(31 * time.Second))
	got, note, err := e.svc.Get(context.Background(), d.ID)
	if err != nil || note != "" {
		t.Fatalf("Get = %v, note %q", err, note)
	}
	if got.Status != StatusPickedUp || got.LastUpdateSource != SourcePoll || polls != 1 {
		t.Errorf("after poll: status %s source %s polls %d", got.Status, got.LastUpdateSource, polls)
	}

	e.clock.set(t0.Add(40 * time.Second))
	e.svc.Get(context.Background(), d.ID)
	if polls != 1 {
		t.Errorf("polled again %v after the last sync; want no poll within 30s", 9*time.Second)
	}
}

func TestGetSkipsPollWhenFreshOrTerminal(t *testing.T) {
	polls := 0
	alpha := &dispatchtest.Partner{PartnerName: "alpha", GetFn: func(_ context.Context, id string) (partner.Delivery, error) {
		polls++
		return partner.Delivery{ID: id, Status: "delivered", Sequence: 4}, nil
	}}
	e := newEnv(t, DefaultConfig(), dispatchtest.Router{}, alpha)
	d, _, _ := e.svc.Create(context.Background(), quoteOne(t, e).ID)

	e.svc.Get(context.Background(), d.ID) // fresh: no poll
	e.clock.add(time.Minute)
	e.svc.Get(context.Background(), d.ID) // stale: poll → delivered
	e.clock.add(time.Minute)
	e.svc.Get(context.Background(), d.ID) // terminal: no poll
	if polls != 1 {
		t.Errorf("polls = %d, want 1", polls)
	}
}

func TestGetPollFailureIsNote(t *testing.T) {
	alpha := &dispatchtest.Partner{PartnerName: "alpha", GetFn: func(context.Context, string) (partner.Delivery, error) {
		return partner.Delivery{}, &partner.APIError{Partner: "alpha", StatusCode: 404, Attempts: 1}
	}}
	e := newEnv(t, DefaultConfig(), dispatchtest.Router{}, alpha)
	d, _, _ := e.svc.Create(context.Background(), quoteOne(t, e).ID)
	e.clock.add(time.Minute)

	got, note, err := e.svc.Get(context.Background(), d.ID)
	if err != nil || got.ID != d.ID || !strings.Contains(note, "no longer knows") {
		t.Errorf("Get = %v, note %q", err, note)
	}

	alpha.GetFn = func(context.Context, string) (partner.Delivery, error) {
		return partner.Delivery{}, &partner.APIError{Partner: "alpha", StatusCode: 503, Attempts: 3}
	}
	_, note, err = e.svc.Get(context.Background(), d.ID)
	if err != nil || note != "could not refresh from partner alpha: unavailable (503) after 3 attempts" {
		t.Errorf("Get = %v, note %q", err, note)
	}
}

func TestListLimits(t *testing.T) {
	e := newEnv(t, DefaultConfig(), dispatchtest.Router{}, &dispatchtest.Partner{PartnerName: "alpha"})
	for range 60 {
		e.svc.Create(context.Background(), quoteOne(t, e).ID)
		e.clock.add(time.Second)
	}
	if page, _, _ := e.svc.List("", 0); len(page) != 10 {
		t.Errorf("limit 0 → %d items, want 10", len(page))
	}
	if page, _, _ := e.svc.List("", 500); len(page) != 50 {
		t.Errorf("limit 500 → %d items, want 50", len(page))
	}
}
