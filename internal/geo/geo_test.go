package geo

import (
	"math"
	"strings"
	"testing"
)

var (
	ferryBuilding  = Point{Lat: 37.7955, Lon: -122.3937}
	missionDolores = Point{Lat: 37.7599, Lon: -122.4268}
)

func TestPointValidate(t *testing.T) {
	tests := []struct {
		name    string
		p       Point
		wantErr string
	}{
		{"valid", Point{37.77, -122.42}, ""},
		{"swapped lat/lon", Point{-122.42, 37.77}, "lat must be between -90 and 90"},
		{"lon too big", Point{0, 181}, "lon must be between -180 and 180"},
		{"edges are valid", Point{-90, 180}, ""},
		{"NaN lat", Point{math.NaN(), 0}, "lat must be"},
		{"Inf lon", Point{0, math.Inf(1)}, "lon must be"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.p.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Errorf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("Validate() = %v, want error containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestDistanceKm(t *testing.T) {
	if d := DistanceKm(ferryBuilding, missionDolores); math.Abs(d-4.9) > 0.2 {
		t.Errorf("Ferry Building to Mission Dolores = %.2f km, want about 4.9", d)
	}
	if d := DistanceKm(ferryBuilding, ferryBuilding); d != 0 {
		t.Errorf("same point = %v, want 0", d)
	}
	if a, b := DistanceKm(ferryBuilding, missionDolores), DistanceKm(missionDolores, ferryBuilding); math.Abs(a-b) > 1e-9 {
		t.Errorf("distance is not symmetric: %v vs %v", a, b)
	}
}
