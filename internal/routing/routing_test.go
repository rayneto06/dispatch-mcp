package routing

import (
	"context"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"golang.org/x/time/rate"

	"github.com/rayneto06/dispatch-mcp/internal/geo"
)

var (
	ferryBuilding  = geo.Point{Lat: 37.7955, Lon: -122.3937}
	missionDolores = geo.Point{Lat: 37.7599, Lon: -122.4268}
)

func newOSRM(t *testing.T, h http.HandlerFunc) *OSRM {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return NewOSRM(srv.URL, srv.Client(), rate.NewLimiter(rate.Inf, 1))
}

func TestOSRMRoutePathIsLonLat(t *testing.T) {
	o := newOSRM(t, func(w http.ResponseWriter, r *http.Request) {
		if want := "/route/v1/driving/-122.3937,37.7955;-122.4268,37.7599"; r.URL.Path != want {
			t.Errorf("path = %s, want %s", r.URL.Path, want)
		}
		if r.URL.Query().Get("overview") != "false" {
			t.Errorf("query = %s, want overview=false", r.URL.RawQuery)
		}
		if r.Header.Get("User-Agent") != UserAgent {
			t.Errorf("User-Agent = %q", r.Header.Get("User-Agent"))
		}
		io.WriteString(w, `{"code":"Ok","routes":[{"distance":4321.5,"duration":610.2}]}`)
	})

	got, err := o.Route(context.Background(), ferryBuilding, missionDolores)
	if err != nil {
		t.Fatal(err)
	}
	if got.DistanceM != 4321.5 || got.Duration != 610200*time.Millisecond || got.Source != SourceOSRM {
		t.Errorf("route = %+v", got)
	}
}

func TestOSRMErrors(t *testing.T) {
	tests := []struct {
		name string
		h    http.HandlerFunc
	}{
		{"500", func(w http.ResponseWriter, r *http.Request) { http.Error(w, "boom", 500) }},
		{"no route", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{"code":"NoRoute","routes":[]}`) }},
		{"ok without routes", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{"code":"Ok","routes":[]}`) }},
		{"invalid JSON", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `not json`) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := newOSRM(t, tt.h).Route(context.Background(), ferryBuilding, missionDolores); err == nil {
				t.Error("want an error")
			}
		})
	}
}

func TestOSRMLimiterWaitRespectsDeadline(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"code":"Ok","routes":[{"distance":1,"duration":1}]}`)
	}))
	defer srv.Close()
	o := NewOSRM(srv.URL, srv.Client(), rate.NewLimiter(1, 1))

	if _, err := o.Route(context.Background(), ferryBuilding, missionDolores); err != nil {
		t.Fatal(err) // uses the only token
	}
	// The next token is about 1s away and only 500ms are left: Wait must
	// fail at once rather than sleep until the deadline.
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := o.Route(ctx, ferryBuilding, missionDolores); err == nil {
		t.Error("second call within a second should fail on the 500ms deadline")
	}
	if time.Since(start) > 400*time.Millisecond {
		t.Errorf("waited %v; the limiter must not wait for a token past the deadline", time.Since(start))
	}
}

func TestEstimate(t *testing.T) {
	r := Estimate(ferryBuilding, missionDolores)
	km := geo.DistanceKm(ferryBuilding, missionDolores) * 1.3
	if math.Abs(r.DistanceM-km*1000) > 1 {
		t.Errorf("distance = %v m, want %v", r.DistanceM, km*1000)
	}
	wantDur := time.Duration(km / 25 * float64(time.Hour))
	if d := r.Duration - wantDur; d > time.Second || d < -time.Second {
		t.Errorf("duration = %v, want about %v", r.Duration, wantDur)
	}
	if r.Source != SourceEstimate {
		t.Errorf("source = %q", r.Source)
	}
}

func TestOSRMSmallCoordinatesHaveNoExponent(t *testing.T) {
	o := newOSRM(t, func(w http.ResponseWriter, r *http.Request) {
		if want := "/route/v1/driving/0.00002,0.00001;-0.00002,-0.00001"; r.URL.Path != want {
			t.Errorf("path = %s, want %s", r.URL.Path, want)
		}
		io.WriteString(w, `{"code":"Ok","routes":[{"distance":1,"duration":1}]}`)
	})
	o.Route(context.Background(), geo.Point{Lat: 0.00001, Lon: 0.00002}, geo.Point{Lat: -0.00001, Lon: -0.00002})
}
