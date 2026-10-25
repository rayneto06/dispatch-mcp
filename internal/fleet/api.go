package fleet

import (
	"encoding/json"
	"io"
	"math"
	"net/http"
	"time"

	"github.com/rayneto06/dispatch-mcp/internal/geo"
	"github.com/rayneto06/dispatch-mcp/internal/obs"
)

// Wire types of the partner API. dispatch-mcp has its own copies in
// internal/partner, as it would with a real partner.

type quoteRequest struct {
	Pickup    geo.Point `json:"pickup"`
	Dropoff   geo.Point `json:"dropoff"`
	DistanceM float64   `json:"distance_m"`
}

type quoteResponse struct {
	PriceCents int    `json:"price_cents"`
	Currency   string `json:"currency"`
	PickupETAS int    `json:"pickup_eta_s"`
	ValidForS  int    `json:"valid_for_s"`
}

type createRequest struct {
	Pickup          geo.Point `json:"pickup"`
	Dropoff         geo.Point `json:"dropoff"`
	PriceCents      int       `json:"price_cents"`
	ClientReference string    `json:"client_reference"`
}

type deliveryResponse struct {
	ID        string    `json:"id"`
	Status    string    `json:"status"`
	Sequence  int64     `json:"sequence"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

const quoteValidFor = 10 * time.Minute

// Handler returns the HTTP API of every partner, plus GET /health.
func (f *Fleet) Handler() http.Handler {
	mux := http.NewServeMux()
	health := func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok\n") }
	mux.HandleFunc("GET /health", health)
	mux.HandleFunc("GET /{partner}/v1/health", health)
	mux.HandleFunc("POST /{partner}/v1/quotes", f.api(f.quote, true))
	mux.HandleFunc("POST /{partner}/v1/deliveries", f.api(f.create, false))
	mux.HandleFunc("GET /{partner}/v1/deliveries/{id}", f.api(f.get, false))
	return mux
}

type apiHandler func(w http.ResponseWriter, r *http.Request, p *partnerState)

// api resolves the partner, checks the API key and simulates latency
// before calling h.
func (f *Fleet) api(h apiHandler, isQuote bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := f.partners[r.PathValue("partner")]
		if !ok {
			writeError(w, http.StatusNotFound, "unknown partner")
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+f.cfg.APIKey {
			writeError(w, http.StatusUnauthorized, "invalid API key")
			return
		}
		f.cfg.Logger.Info("partner request", "partner", p.profile.Name, "method", r.Method,
			"path", r.URL.Path, "request_id", r.Header.Get(obs.RequestIDHeader))

		latency := p.profile.Latency
		if isQuote {
			latency = p.profile.QuoteLatency
		}
		if d := f.between(latency); d > 0 {
			select {
			case <-time.After(d):
			case <-r.Context().Done(): // caller gave up; don't do the work
				return
			}
		}

		switch roll := f.cfg.Rand(); {
		case roll < p.profile.Rate429:
			w.Header().Set("Retry-After", "1")
			writeError(w, http.StatusTooManyRequests, "rate limited")
			return
		case roll < p.profile.Rate429+p.profile.Rate503:
			writeError(w, http.StatusServiceUnavailable, "temporarily unavailable")
			return
		}
		h(w, r, p)
	}
}

func (f *Fleet) quote(w http.ResponseWriter, r *http.Request, p *partnerState) {
	var req quoteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.DistanceM < 0 {
		writeError(w, http.StatusBadRequest, "invalid quote request")
		return
	}
	price := p.profile.BaseCents + int(math.Round(float64(p.profile.PerKmCents)*req.DistanceM/1000))
	writeJSON(w, http.StatusOK, quoteResponse{
		PriceCents: price,
		Currency:   "USD",
		PickupETAS: int(f.between(p.profile.PickupETA).Seconds()),
		ValidForS:  int(quoteValidFor.Seconds()),
	})
}

func (f *Fleet) create(w http.ResponseWriter, r *http.Request, p *partnerState) {
	key := r.Header.Get("Idempotency-Key")
	if key == "" {
		writeError(w, http.StatusBadRequest, "Idempotency-Key header is required")
		return
	}
	var req createRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid delivery request")
		return
	}

	f.mu.Lock()
	if prev, ok := p.byKey[key]; ok {
		f.mu.Unlock()
		if prev.request != req {
			writeError(w, http.StatusUnprocessableEntity, "Idempotency-Key reused with a different request")
			return
		}
		w.Header().Set("Idempotent-Replayed", "true")
		writeJSON(w, http.StatusCreated, prev.resp)
		return
	}
	if f.closed {
		f.mu.Unlock()
		writeError(w, http.StatusServiceUnavailable, "shutting down")
		return
	}
	now := time.Now().UTC()
	d := &deliveryResponse{
		ID:        p.profile.Name + "_" + randomHex(6),
		Status:    "pending",
		Sequence:  1,
		CreatedAt: now,
		UpdatedAt: now,
	}
	p.deliveries[d.ID] = d
	resp := *d // copy under the lock: advance mutates *d once it starts
	p.byKey[key] = idempotentEntry{request: req, resp: resp}
	f.progress.Add(1) // under f.mu, so it can't race Shutdown's Wait
	f.mu.Unlock()

	go f.advance(p.profile.Name, resp.ID)

	// The booking is made, but the caller never hears so. Only a retry with
	// the same Idempotency-Key can recover it without booking twice.
	if f.cfg.Rand() < p.profile.CommitThenFail {
		writeError(w, http.StatusServiceUnavailable, "temporarily unavailable")
		return
	}
	writeJSON(w, http.StatusCreated, resp)
}

func (f *Fleet) get(w http.ResponseWriter, r *http.Request, p *partnerState) {
	f.mu.Lock()
	d, ok := p.deliveries[r.PathValue("id")]
	var resp deliveryResponse
	if ok {
		resp = *d // copy while holding the lock
	}
	f.mu.Unlock()
	if !ok {
		writeError(w, http.StatusNotFound, "delivery not found")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
