package model

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
)

// The canonical geographic representation across the whole server is an
// RFC 7946 GeoJSON geometry: WGS84, positions as [longitude, latitude],
// longitude in [-180, 180], latitude in [-90, 90]. Every source's shapes
// (circles, polygons, multi-part areas, antimeridian-crossing areas) are
// translated into one of five types: Point, MultiPoint, LineString, Polygon,
// MultiPolygon. Translation lives in internal/geo and is deterministic.

// Position is a WGS84 position: [longitude, latitude] in decimal degrees.
// The order is GeoJSON's, not the more common "lat, lon".
type Position [2]float64

func (p Position) Lon() float64 { return p[0] }
func (p Position) Lat() float64 { return p[1] }

func (p Position) validate() error {
	lon, lat := p[0], p[1]
	if math.IsNaN(lon) || math.IsNaN(lat) || math.IsInf(lon, 0) || math.IsInf(lat, 0) {
		return errors.New("position is not finite")
	}
	if lon < -180 || lon > 180 {
		return fmt.Errorf("longitude %v outside [-180, 180]", lon)
	}
	if lat < -90 || lat > 90 {
		return fmt.Errorf("latitude %v outside [-90, 90]", lat)
	}
	return nil
}

// GeometryType is one of the four canonical GeoJSON geometry types.
type GeometryType string

const (
	GeomPoint        GeometryType = "Point"
	GeomMultiPoint   GeometryType = "MultiPoint"
	GeomLineString   GeometryType = "LineString"
	GeomPolygon      GeometryType = "Polygon"
	GeomMultiPolygon GeometryType = "MultiPolygon"
)

// Geometry is a canonical geometry. Exactly the field matching Type is set.
// Polygons holds one polygon for Polygon and several for MultiPolygon; each
// polygon is a list of rings, the exterior ring first, then any holes. Rings
// are closed and wound per RFC 7946 (exterior counter-clockwise, holes
// clockwise).
type Geometry struct {
	Type     GeometryType
	Point    Position
	Points   []Position // MultiPoint, exactly as published (duplicates kept)
	Line     []Position
	Polygons [][][]Position
}

func NewPoint(lon, lat float64) Geometry { return Geometry{Type: GeomPoint, Point: Position{lon, lat}} }
func NewMultiPoint(pts []Position) Geometry {
	return Geometry{Type: GeomMultiPoint, Points: pts}
}
func NewLineString(pts []Position) Geometry {
	return Geometry{Type: GeomLineString, Line: pts}
}

// NewPolygons returns a Polygon for one polygon and a MultiPolygon for several.
func NewPolygons(polys [][][]Position) Geometry {
	if len(polys) == 1 {
		return Geometry{Type: GeomPolygon, Polygons: polys}
	}
	return Geometry{Type: GeomMultiPolygon, Polygons: polys}
}

type geoJSON struct {
	Type        GeometryType `json:"type"`
	Coordinates any          `json:"coordinates"`
}

// MarshalJSON emits an RFC 7946 geometry object.
func (g Geometry) MarshalJSON() ([]byte, error) {
	out := geoJSON{Type: g.Type}
	switch g.Type {
	case GeomPoint:
		out.Coordinates = g.Point
	case GeomMultiPoint:
		out.Coordinates = g.Points
	case GeomLineString:
		out.Coordinates = g.Line
	case GeomPolygon:
		if len(g.Polygons) != 1 {
			return nil, errors.New("geometry: Polygon must hold exactly one polygon")
		}
		out.Coordinates = g.Polygons[0]
	case GeomMultiPolygon:
		out.Coordinates = g.Polygons
	default:
		return nil, fmt.Errorf("geometry: unknown type %q", g.Type)
	}
	return json.Marshal(out)
}

// UnmarshalJSON reads an RFC 7946 geometry object.
func (g *Geometry) UnmarshalJSON(b []byte) error {
	var raw struct {
		Type        GeometryType    `json:"type"`
		Coordinates json.RawMessage `json:"coordinates"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	*g = Geometry{Type: raw.Type}
	switch raw.Type {
	case GeomPoint:
		return json.Unmarshal(raw.Coordinates, &g.Point)
	case GeomMultiPoint:
		return json.Unmarshal(raw.Coordinates, &g.Points)
	case GeomLineString:
		return json.Unmarshal(raw.Coordinates, &g.Line)
	case GeomPolygon:
		var rings [][]Position
		if err := json.Unmarshal(raw.Coordinates, &rings); err != nil {
			return err
		}
		g.Polygons = [][][]Position{rings}
		return nil
	case GeomMultiPolygon:
		return json.Unmarshal(raw.Coordinates, &g.Polygons)
	}
	return fmt.Errorf("geometry: unsupported type %q", raw.Type)
}

// SignedArea is the planar signed area of a ring in lon/lat space: positive
// for counter-clockwise. It is used only to check winding.
func SignedArea(ring []Position) float64 {
	var a float64
	for i := 0; i+1 < len(ring); i++ {
		a += ring[i][0]*ring[i+1][1] - ring[i+1][0]*ring[i][1]
	}
	return a / 2
}

// Validate checks the canonical invariants: finite in-range positions,
// closed rings of at least four positions, RFC 7946 winding, a non-empty
// shape of the declared type.
func (g Geometry) Validate() error {
	switch g.Type {
	case GeomPoint:
		return g.Point.validate()
	case GeomMultiPoint:
		if len(g.Points) == 0 {
			return errors.New("geometry: MultiPoint needs at least 1 position")
		}
		for i, p := range g.Points {
			if err := p.validate(); err != nil {
				return fmt.Errorf("geometry: point %d: %w", i, err)
			}
		}
		return nil
	case GeomLineString:
		if len(g.Line) < 2 {
			return errors.New("geometry: LineString needs at least 2 positions")
		}
		for i, p := range g.Line {
			if err := p.validate(); err != nil {
				return fmt.Errorf("geometry: line position %d: %w", i, err)
			}
		}
		return nil
	case GeomPolygon, GeomMultiPolygon:
		if g.Type == GeomPolygon && len(g.Polygons) != 1 {
			return errors.New("geometry: Polygon must hold exactly one polygon")
		}
		if len(g.Polygons) == 0 {
			return errors.New("geometry: no polygons")
		}
		for pi, poly := range g.Polygons {
			if len(poly) == 0 {
				return fmt.Errorf("geometry: polygon %d has no rings", pi)
			}
			for ri, ring := range poly {
				if len(ring) < 4 || ring[0] != ring[len(ring)-1] {
					return fmt.Errorf("geometry: polygon %d ring %d is not a closed ring of at least 4 positions", pi, ri)
				}
				for _, p := range ring {
					if err := p.validate(); err != nil {
						return fmt.Errorf("geometry: polygon %d ring %d: %w", pi, ri, err)
					}
				}
				area := SignedArea(ring)
				if area == 0 {
					return fmt.Errorf("geometry: polygon %d ring %d has zero area", pi, ri)
				}
				if (ri == 0) != (area > 0) {
					return fmt.Errorf("geometry: polygon %d ring %d is wound the wrong way for RFC 7946", pi, ri)
				}
			}
		}
		return nil
	}
	return fmt.Errorf("geometry: unknown type %q", g.Type)
}

// Circle is a circular area exactly as a source published it.
type Circle struct {
	Center   Position `json:"center"`
	RadiusNM float64  `json:"radius_nm"`
}

// Derivation records how a Geometry was produced when it is not simply the
// source's own shape, so a caller can see (and undo) every translation.
type Derivation struct {
	// Steps names each translation applied, in order, e.g.
	// "circle_to_circumscribed_polygon_64", "antimeridian_cut",
	// "antimeridian_shift", "ring_reoriented".
	Steps []string `json:"steps"`
	// Circle is the source's circle when the geometry was derived from one.
	Circle *Circle `json:"circle,omitempty"`
}
