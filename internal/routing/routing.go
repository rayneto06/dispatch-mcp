// Package routing gets driving distance and time between two points from
// OSRM (a public routing API we don't control), with a straight-line
// estimate for when it is unavailable.
package routing

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"golang.org/x/time/rate"

	"github.com/rayneto06/dispatch-mcp/internal/geo"
)

const (
	SourceOSRM     = "osrm"
	SourceEstimate = "estimate"
)

// UserAgent identifies us to the OSRM demo server, as its usage policy asks.
const UserAgent = "dispatch-mcp/0.1 (+https://github.com/rayneto06/dispatch-mcp)"

type Route struct {
	DistanceM float64
	Duration  time.Duration
	Source    string
}

type OSRM struct {
	baseURL string
	http    *http.Client
	limiter *rate.Limiter
}

// NewOSRM returns a client for an OSRM server. limiter is shared by every
// call; the public demo server allows at most one request per second.
func NewOSRM(baseURL string, httpClient *http.Client, limiter *rate.Limiter) *OSRM {
	return &OSRM{baseURL: baseURL, http: httpClient, limiter: limiter}
}

// Route asks OSRM for the driving route from one point to another.
func (o *OSRM) Route(ctx context.Context, from, to geo.Point) (Route, error) {
	// Wait returns at once if waiting would pass ctx's deadline.
	if err := o.limiter.Wait(ctx); err != nil {
		return Route{}, fmt.Errorf("osrm rate limit: %w", err)
	}
	// OSRM takes lon,lat, the reverse of how people write coordinates.
	url := fmt.Sprintf("%s/route/v1/driving/%s,%s;%s,%s?overview=false", o.baseURL, num(from.Lon), num(from.Lat), num(to.Lon), num(to.Lat))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Route{}, err
	}
	req.Header.Set("User-Agent", UserAgent)

	resp, err := o.http.Do(req)
	if err != nil {
		return Route{}, fmt.Errorf("osrm: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Route{}, fmt.Errorf("osrm: status %d", resp.StatusCode)
	}

	var body struct {
		Code   string `json:"code"`
		Routes []struct {
			Distance float64 `json:"distance"` // meters
			Duration float64 `json:"duration"` // seconds
		} `json:"routes"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return Route{}, fmt.Errorf("osrm: decode: %w", err)
	}
	if body.Code != "Ok" || len(body.Routes) == 0 {
		return Route{}, fmt.Errorf("osrm: no route (code %q)", body.Code)
	}
	r := body.Routes[0]
	return Route{
		DistanceM: r.Distance,
		Duration:  time.Duration(r.Duration * float64(time.Second)),
		Source:    SourceOSRM,
	}, nil
}

// num formats a coordinate without exponent notation (1e-05 would not parse).
func num(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

// Estimate approximates a city route from the straight-line distance:
// roads are about 30% longer than the crow flies, at about 25 km/h.
func Estimate(from, to geo.Point) Route {
	km := geo.DistanceKm(from, to) * 1.3
	return Route{
		DistanceM: km * 1000,
		Duration:  time.Duration(km / 25 * float64(time.Hour)),
		Source:    SourceEstimate,
	}
}
