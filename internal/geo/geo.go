// Package geo has the one geographic type the service needs and the
// straight-line distance between two points.
package geo

import (
	"fmt"
	"math"
)

// Point is a WGS84 coordinate. The jsonschema tags become the descriptions
// an agent sees in the MCP tool schema.
type Point struct {
	Lat float64 `json:"lat" jsonschema:"latitude in decimal degrees, -90 to 90"`
	Lon float64 `json:"lon" jsonschema:"longitude in decimal degrees, -180 to 180"`
}

// Validate reports a coordinate outside the valid range. Swapped lat/lon is
// the common agent mistake, so the message names the field and its range.
func (p Point) Validate() error {
	if math.IsNaN(p.Lat) || p.Lat < -90 || p.Lat > 90 {
		return fmt.Errorf("lat must be between -90 and 90, got %v", p.Lat)
	}
	if math.IsNaN(p.Lon) || p.Lon < -180 || p.Lon > 180 {
		return fmt.Errorf("lon must be between -180 and 180, got %v", p.Lon)
	}
	return nil
}

const earthRadiusKm = 6371.0088 // mean Earth radius

// DistanceKm is the great-circle (haversine) distance between a and b.
func DistanceKm(a, b Point) float64 {
	rad := func(deg float64) float64 { return deg * math.Pi / 180 }
	dLat := rad(b.Lat - a.Lat)
	dLon := rad(b.Lon - a.Lon)
	h := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(rad(a.Lat))*math.Cos(rad(b.Lat))*math.Sin(dLon/2)*math.Sin(dLon/2)
	return 2 * earthRadiusKm * math.Asin(math.Sqrt(h))
}
