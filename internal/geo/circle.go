package geo

import (
	"errors"
	"fmt"
	"math"

	"github.com/davidhinkes/hobbs/internal/model"
)

const (
	// CircleVertices is the number of vertices in a translated circle. The
	// polygon is circumscribed (every edge lies outside the true circle) and
	// then padded by circleSafety, so it is slightly LARGER than the circle:
	// about 0.2% in radius in total (0.01 nm on a 5 nm circle). Overstating
	// shows as extra data; understating would hide it, so we err on the larger
	// side. The padding also covers the difference between a straight line in
	// lon/lat space (what GeoJSON edges are) and a geodesic, which is
	// largest at high latitudes.
	CircleVertices = 64
	circleSafety   = 1.001
	// maxCircleLat refuses circles that reach beyond this latitude: close to a
	// pole, lon/lat space is too distorted for a faithful polygon.
	maxCircleLat = 85.0
	// MaxCircleNM bounds circles; beyond this the shape is not a local area.
	MaxCircleNM = 500
	metersPerNM = 1852.0

	// WGS84
	wgsA = 6378137.0
	wgsF = 1 / 298.257223563
)

// ErrCircle: the circle cannot be translated to a polygon.
var ErrCircle = errors.New("circle cannot be translated")

func rad(d float64) float64 { return d * math.Pi / 180 }
func deg(r float64) float64 { return r * 180 / math.Pi }

// vincentyDirect solves the geodesic direct problem on the WGS84 ellipsoid:
// from (lat1, lon1) degrees, initial bearing alpha1 degrees, distance s
// meters, returns the destination in degrees (longitude not wrapped).
func vincentyDirect(lat1, lon1, alpha1, s float64) (lat2, lon2 float64) {
	a, f := wgsA, wgsF
	b := (1 - f) * a
	phi1, L1, al1 := rad(lat1), rad(lon1), rad(alpha1)
	sinAl1, cosAl1 := math.Sincos(al1)
	tanU1 := (1 - f) * math.Tan(phi1)
	cosU1 := 1 / math.Sqrt(1+tanU1*tanU1)
	sinU1 := tanU1 * cosU1
	sigma1 := math.Atan2(tanU1, cosAl1)
	sinAl := cosU1 * sinAl1
	cos2Al := 1 - sinAl*sinAl
	u2 := cos2Al * (a*a - b*b) / (b * b)
	A := 1 + u2/16384*(4096+u2*(-768+u2*(320-175*u2)))
	B := u2 / 1024 * (256 + u2*(-128+u2*(74-47*u2)))
	sigma := s / (b * A)
	var cos2sm, sinSig, cosSig float64
	for i := 0; i < 100; i++ {
		cos2sm = math.Cos(2*sigma1 + sigma)
		sinSig, cosSig = math.Sincos(sigma)
		dSigma := B * sinSig * (cos2sm + B/4*(cosSig*(-1+2*cos2sm*cos2sm)-
			B/6*cos2sm*(-3+4*sinSig*sinSig)*(-3+4*cos2sm*cos2sm)))
		next := s/(b*A) + dSigma
		if math.Abs(next-sigma) < 1e-12 {
			sigma = next
			break
		}
		sigma = next
	}
	cos2sm = math.Cos(2*sigma1 + sigma)
	sinSig, cosSig = math.Sincos(sigma)
	x := sinU1*sinSig - cosU1*cosSig*cosAl1
	phi2 := math.Atan2(sinU1*cosSig+cosU1*sinSig*cosAl1, (1-f)*math.Sqrt(sinAl*sinAl+x*x))
	lambda := math.Atan2(sinSig*sinAl1, cosU1*cosSig-sinU1*sinSig*cosAl1)
	C := f / 16 * cos2Al * (4 + f*(4-3*cos2Al))
	L := lambda - (1-C)*f*sinAl*(sigma+C*sinSig*(cos2sm+C*cosSig*(-1+2*cos2sm*cos2sm)))
	return deg(phi2), deg(L1 + L)
}

// wrapDelta maps an angle difference to [-180, 180].
func wrapDelta(d float64) float64 {
	d = math.Mod(d+180, 360)
	if d < 0 {
		d += 360
	}
	return d - 180
}

// Circle translates a circle (center plus radius in nautical miles) into a
// canonical Polygon, or a MultiPolygon when it crosses the antimeridian. The
// polygon is geodesic (WGS84), slightly larger than the circle (see CircleVertices), wound per
// RFC 7946, and starts at the northernmost vertex. A radius of 0 is a Point.
// Circles that come near a pole (see maxCircleLat) or exceed MaxCircleNM are refused.
func Circle(center model.Position, radiusNM float64) (model.Geometry, []string, error) {
	if err := (model.Geometry{Type: model.GeomPoint, Point: center}).Validate(); err != nil {
		return model.Geometry{}, nil, fmt.Errorf("%w: center: %v", ErrCircle, err)
	}
	switch {
	case math.IsNaN(radiusNM) || radiusNM < 0:
		return model.Geometry{}, nil, fmt.Errorf("%w: radius %v", ErrCircle, radiusNM)
	case radiusNM == 0:
		return model.NewPoint(center[0], center[1]), nil, nil
	case radiusNM > MaxCircleNM:
		return model.Geometry{}, nil, fmt.Errorf("%w: radius %v nm exceeds %d nm", ErrCircle, radiusNM, MaxCircleNM)
	}
	// One degree of latitude is at least 59.7 nm anywhere on the ellipsoid; use
	// 59 for a conservative extent with a 10% margin.
	if math.Abs(center[1])+radiusNM/59*1.1 > maxCircleLat {
		return model.Geometry{}, nil, fmt.Errorf("%w: circle comes within %v degrees of a pole", ErrCircle, 90-maxCircleLat)
	}
	dist := radiusNM * metersPerNM / math.Cos(math.Pi/CircleVertices) * circleSafety
	ring := make([]model.Position, 0, CircleVertices+1)
	for k := 0; k < CircleVertices; k++ {
		// Decreasing bearings from north give a counter-clockwise ring.
		bearing := math.Mod(360-float64(k)*360/CircleVertices, 360)
		lat, lon := vincentyDirect(center[1], center[0], bearing, dist)
		// Keep longitudes continuous around the center (may exceed +/-180).
		ring = append(ring, model.Position{center[0] + wrapDelta(lon-center[0]), lat})
	}
	ring = append(ring, ring[0])
	polys, steps, err := NormalizePolygon([][]model.Position{ring})
	if err != nil {
		return model.Geometry{}, nil, fmt.Errorf("%w: %v", ErrCircle, err)
	}
	g := model.NewPolygons(polys)
	if err := g.Validate(); err != nil {
		return model.Geometry{}, nil, fmt.Errorf("%w: %v", ErrCircle, err)
	}
	return g, append([]string{fmt.Sprintf("circle_to_circumscribed_polygon_%d", CircleVertices)}, steps...), nil
}
