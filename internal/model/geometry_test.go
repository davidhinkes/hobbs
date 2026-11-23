package model

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestGeometryMarshalsAsRFC7946(t *testing.T) {
	tests := map[string]struct {
		g    Geometry
		want string
	}{
		"point (lon, lat order)":       {NewPoint(-77.45, 38.95), `{"type":"Point","coordinates":[-77.45,38.95]}`},
		"multipoint (duplicates kept)": {NewMultiPoint([]Position{{1, 2}, {3, 4}, {1, 2}}), `{"type":"MultiPoint","coordinates":[[1,2],[3,4],[1,2]]}`},
		"line":                         {NewLineString([]Position{{0, 0}, {1, 2}}), `{"type":"LineString","coordinates":[[0,0],[1,2]]}`},
		"polygon":                      {NewPolygons([][][]Position{{{{0, 0}, {1, 0}, {1, 1}, {0, 0}}}}), `{"type":"Polygon","coordinates":[[[0,0],[1,0],[1,1],[0,0]]]}`},
		"multipolygon": {NewPolygons([][][]Position{{{{0, 0}, {1, 0}, {1, 1}, {0, 0}}}, {{{5, 5}, {6, 5}, {6, 6}, {5, 5}}}}),
			`{"type":"MultiPolygon","coordinates":[[[[0,0],[1,0],[1,1],[0,0]]],[[[5,5],[6,5],[6,6],[5,5]]]]}`},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			b, err := json.Marshal(tt.g)
			if err != nil || string(b) != tt.want {
				t.Fatalf("got %s (%v), want %s", b, err, tt.want)
			}
			var back Geometry
			if err := json.Unmarshal(b, &back); err != nil {
				t.Fatal(err)
			}
			b2, _ := json.Marshal(back)
			if string(b2) != tt.want {
				t.Errorf("round trip = %s", b2)
			}
		})
	}
	// Inside a record the geometry is the plain GeoJSON object, with the
	// derivation as a sibling.
	g := NewPoint(1, 2)
	a := Advisory{ID: "x", Geometry: &g, GeometryDerivation: &Derivation{Steps: []string{"s"}, Circle: &Circle{Center: Position{1, 2}, RadiusNM: 5}}}
	b, _ := json.Marshal(a)
	for _, want := range []string{`"geometry":{"type":"Point","coordinates":[1,2]}`, `"geometry_derivation":{"steps":["s"],"circle":{"center":[1,2],"radius_nm":5}}`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("missing %s in %s", want, b)
		}
	}
}

func TestGeometryUnmarshalRejectsUnsupported(t *testing.T) {
	for _, in := range []string{`{"type":"GeometryCollection"}`, `{"type":"MultiLineString","coordinates":[[[0,0],[1,1]]]}`, `{"type":"Point","coordinates":"x"}`, `[]`} {
		var g Geometry
		if json.Unmarshal([]byte(in), &g) == nil {
			t.Errorf("%s should fail", in)
		}
	}
	if _, err := json.Marshal(Geometry{Type: "Bogus"}); err == nil {
		t.Error("unknown type must not marshal")
	}
	if _, err := json.Marshal(Geometry{Type: GeomPolygon}); err == nil {
		t.Error("Polygon with no polygon must not marshal")
	}
}

func TestPositionRangeAndWinding(t *testing.T) {
	bad := []Geometry{
		NewPoint(180.0001, 0), NewPoint(0, -90.1), NewPoint(nan(), 0),
		NewLineString([]Position{{0, 0}}),
		NewMultiPoint(nil), NewMultiPoint([]Position{{0, 0}, {200, 0}}),
		NewLineString([]Position{{0, 0}, {0, 91}}),
		{Type: GeomPolygon, Polygons: [][][]Position{{{{0, 0}, {1, 1}, {2, 2}, {0, 0}}}}}, // zero area
		{Type: GeomPolygon, Polygons: nil},
		{Type: GeomMultiPolygon},
	}
	for i, g := range bad {
		if g.Validate() == nil {
			t.Errorf("bad[%d] should fail validation", i)
		}
	}
	// Exactly +/-180 and +/-90 are valid.
	for _, g := range []Geometry{NewPoint(180, 90), NewPoint(-180, -90)} {
		if err := g.Validate(); err != nil {
			t.Error(err)
		}
	}
	// A polygon with a correctly wound hole is valid; a CCW hole is not.
	ext := []Position{{0, 0}, {10, 0}, {10, 10}, {0, 10}, {0, 0}}
	cwHole := []Position{{2, 2}, {2, 4}, {4, 4}, {4, 2}, {2, 2}}
	ccwHole := []Position{{2, 2}, {4, 2}, {4, 4}, {2, 4}, {2, 2}}
	if err := NewPolygons([][][]Position{{ext, cwHole}}).Validate(); err != nil {
		t.Error(err)
	}
	if NewPolygons([][][]Position{{ext, ccwHole}}).Validate() == nil {
		t.Error("counter-clockwise hole must be rejected")
	}
}

func nan() float64 { var z float64; return z / z }
