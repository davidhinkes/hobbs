package geo

import (
	"errors"
	"math"
	"testing"

	"github.com/davidhinkes/hobbs/internal/model"
)

type P = model.Position

func near(t *testing.T, name string, got, want, tol float64) {
	t.Helper()
	if math.Abs(got-want) > tol {
		t.Errorf("%s = %.12f, want %.12f (tol %g)", name, got, want, tol)
	}
}

// pip is a planar ray-casting point-in-ring test in lon/lat space.
func pip(pt P, ring []P) bool {
	in := false
	for i, j := 0, len(ring)-1; i < len(ring); j, i = i, i+1 {
		a, b := ring[i], ring[j]
		if (a[1] > pt[1]) != (b[1] > pt[1]) && pt[0] < (b[0]-a[0])*(pt[1]-a[1])/(b[1]-a[1])+a[0] {
			in = !in
		}
	}
	return in
}

func inGeometry(pt P, g model.Geometry) bool {
	for _, poly := range g.Polygons {
		if !pip(pt, poly[0]) {
			continue
		}
		hole := false
		for _, h := range poly[1:] {
			if pip(pt, h) {
				hole = true
			}
		}
		if !hole {
			return true
		}
	}
	return false
}

func totalArea(g model.Geometry) float64 {
	var a float64
	for _, poly := range g.Polygons {
		for i, r := range poly {
			if i == 0 {
				a += math.Abs(model.SignedArea(r))
			} else {
				a -= math.Abs(model.SignedArea(r))
			}
		}
	}
	return a
}

func TestVincentyKnownValues(t *testing.T) {
	// Along the equator the geodesic is the equator: 1 degree = a*pi/180 metres.
	lat, lon := vincentyDirect(0, 0, 90, wgsA*math.Pi/180)
	near(t, "equator east lat", lat, 0, 1e-9)
	near(t, "equator east lon", lon, 1, 1e-9)
	// 1 degree of meridian arc at the equator is 110574.2727 m.
	lat, lon = vincentyDirect(0, 0, 0, 110574.2727)
	near(t, "meridian lat", lat, 1, 1e-5)
	near(t, "meridian lon", lon, 0, 1e-9)
	// Geoscience Australia worked example (Flinders Peak to Buninyong, direct).
	lat, lon = vincentyDirect(-37.95103341666667, 144.42486788888890, 306.868158333333, 54972.271)
	near(t, "Flinders lat", lat, -37.65282113888889, 1e-7)
	near(t, "Flinders lon", lon, 143.92649552777778, 1e-7)
}

func TestCircleContainsTheTrueCircle(t *testing.T) {
	for _, c := range []P{{-77.45, 38.95}, {0, 0}, {10, 60}, {-150, -45}, {120, 20}, {-30, 80}} {
		for _, r := range []float64{1, 5, 25, 100} {
			g, steps, err := Circle(c, r)
			if err != nil {
				t.Fatalf("%v r=%v: %v", c, r, err)
			}
			if g.Type != model.GeomPolygon || len(g.Polygons[0][0]) != CircleVertices+1 {
				t.Fatalf("%v r=%v: %+v", c, r, g.Type)
			}
			if len(steps) != 1 || steps[0] != "circle_to_circumscribed_polygon_64" {
				t.Errorf("steps = %v", steps)
			}
			// Every point of the true circle must be inside the polygon.
			for b := 0.0; b < 360; b += 0.5 {
				lat, lon := vincentyDirect(c[1], c[0], b, r*metersPerNM)
				pt := P{c[0] + wrapDelta(lon-c[0]), lat}
				if !pip(pt, g.Polygons[0][0]) {
					t.Fatalf("%v r=%v: true circle point at bearing %v lies outside the polygon", c, r, b)
				}
			}
			if err := g.Validate(); err != nil {
				t.Errorf("invalid: %v", err)
			}
		}
	}
}

func TestCircleIsDeterministicAndStartsNorth(t *testing.T) {
	a, _, _ := Circle(P{-77.45, 38.95}, 25)
	b, _, _ := Circle(P{-77.45, 38.95}, 25)
	for i := range a.Polygons[0][0] {
		if a.Polygons[0][0][i] != b.Polygons[0][0][i] {
			t.Fatal("same input must give identical output")
		}
	}
	first := a.Polygons[0][0][0]
	near(t, "first vertex lon (due north of center)", first[0], -77.45, 1e-9)
	if first[1] <= 38.95 {
		t.Errorf("first vertex should be north of the center: %v", first)
	}
	if model.SignedArea(a.Polygons[0][0]) <= 0 {
		t.Error("exterior ring must be counter-clockwise")
	}
}

func TestCircleCrossingTheAntimeridian(t *testing.T) {
	for _, c := range []P{{179.95, 10}, {-179.95, 10}, {180, -20}} {
		g, steps, err := Circle(c, 25)
		if err != nil {
			t.Fatal(err)
		}
		if g.Type != model.GeomMultiPolygon || len(g.Polygons) != 2 {
			t.Fatalf("%v: got %s with %d polygons", c, g.Type, len(g.Polygons))
		}
		found := false
		for _, s := range steps {
			if s == StepAntimeridianCut {
				found = true
			}
		}
		if !found {
			t.Errorf("steps = %v", steps)
		}
		if err := g.Validate(); err != nil {
			t.Errorf("invalid: %v", err)
		}
		for b := 0.0; b < 360; b += 0.5 {
			lat, lon := vincentyDirect(c[1], c[0], b, 25*metersPerNM)
			pt := P{wrapDelta(lon), lat} // canonical longitude
			if !inGeometry(pt, g) {
				t.Fatalf("%v: true circle point at bearing %v (%v) is outside the cut polygons", c, b, pt)
			}
		}
	}
}

func TestCircleRefusals(t *testing.T) {
	if g, _, err := Circle(P{-77, 38}, 0); err != nil || g.Type != model.GeomPoint {
		t.Errorf("radius 0 is a point: %v %v", g.Type, err)
	}
	for name, c := range map[string]struct {
		c P
		r float64
	}{
		"negative":       {P{0, 0}, -1},
		"NaN":            {P{0, 0}, math.NaN()},
		"too large":      {P{0, 0}, MaxCircleNM + 1},
		"near pole":      {P{0, 89.5}, 25},
		"over pole":      {P{0, 89}, 100},
		"high latitude":  {P{0, 84.9}, 10},
		"bad center lat": {P{0, 91}, 5},
		"bad center lon": {P{181, 0}, 5},
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := Circle(c.c, c.r); !errors.Is(err, ErrCircle) {
				t.Errorf("err = %v, want ErrCircle", err)
			}
		})
	}
}

func rect(x0, y0, x1, y1 float64) []P {
	return []P{{x0, y0}, {x1, y0}, {x1, y1}, {x0, y1}, {x0, y0}}
}

func TestNormalizeInRangeAndWinding(t *testing.T) {
	ccw := rect(0, 0, 10, 10)
	polys, steps, err := NormalizePolygon([][]P{ccw})
	if err != nil || len(polys) != 1 || len(steps) != 0 {
		t.Fatalf("polys=%d steps=%v err=%v", len(polys), steps, err)
	}
	cw := reversed(ccw)
	polys, steps, err = NormalizePolygon([][]P{cw})
	if err != nil || len(steps) != 1 || steps[0] != StepReoriented || model.SignedArea(polys[0][0]) <= 0 {
		t.Fatalf("clockwise exterior must be reoriented: steps=%v err=%v", steps, err)
	}
	// Holes must be clockwise; a counter-clockwise hole is reversed.
	hole := rect(2, 2, 4, 4) // CCW
	polys, steps, err = NormalizePolygon([][]P{ccw, hole})
	if err != nil || model.SignedArea(polys[0][1]) >= 0 || len(steps) != 1 {
		t.Fatalf("hole winding: steps=%v err=%v area=%v", steps, err, model.SignedArea(polys[0][1]))
	}
	g := model.NewPolygons(polys)
	if err := g.Validate(); err != nil {
		t.Error(err)
	}
}

func TestNormalizeShiftsRingsWhollyBeyondTheAntimeridian(t *testing.T) {
	// Shaped like the NZKL 22 second part from the live feed: starts on 180 and runs east.
	ring := []P{{180, -72.084}, {180, -73.931}, {183.5, -73.667}, {196.333, -66.167}, {195.833, -58.833}, {188.5, -68.5}, {180, -72.084}}
	polys, steps, err := NormalizePolygon([][]P{ring})
	if err != nil || len(polys) != 1 {
		t.Fatalf("polys=%d err=%v", len(polys), err)
	}
	if len(steps) == 0 || steps[0] != StepAntimeridianShift {
		t.Errorf("steps = %v", steps)
	}
	min, max := lonRange(polys[0][0])
	near(t, "min lon", min, -180, 1e-12)
	near(t, "max lon", max, 196.333-360, 1e-12)
	g := model.NewPolygons(polys)
	if err := g.Validate(); err != nil {
		t.Error(err)
	}
	near(t, "area preserved", totalArea(g), math.Abs(model.SignedArea(ring)), 1e-9)
	// And the western mirror image shifts the other way.
	west := shift([][]P{ring}, -360)[0]
	polys, _, err = NormalizePolygon([][]P{west})
	if err != nil {
		t.Fatal(err)
	}
	min, max = lonRange(polys[0][0])
	near(t, "mirror min", min, -180, 1e-12)
	near(t, "mirror max", max, 196.333-360, 1e-12)
}

func TestNormalizeCutsStraddlingRingsAndConservesArea(t *testing.T) {
	cases := map[string][]P{
		"east rectangle":  rect(170, 0, 190, 10),
		"west rectangle":  rect(-190, 0, -170, 10),
		"east triangle":   {{175, 0}, {195, 0}, {185, 20}, {175, 0}},
		"concave U shape": {{170, 0}, {190, 0}, {190, 10}, {185, 10}, {185, 4}, {175, 4}, {175, 10}, {170, 10}, {170, 0}},
	}
	for name, ring := range cases {
		t.Run(name, func(t *testing.T) {
			polys, steps, err := NormalizePolygon([][]P{ring})
			if err != nil {
				t.Fatal(err)
			}
			if len(polys) != 2 {
				t.Fatalf("polygons = %d, want 2", len(polys))
			}
			found := false
			for _, s := range steps {
				found = found || s == StepAntimeridianCut
			}
			if !found {
				t.Errorf("steps = %v", steps)
			}
			g := model.NewPolygons(polys)
			if err := g.Validate(); err != nil {
				t.Fatal(err)
			}
			near(t, "area conserved", totalArea(g), math.Abs(model.SignedArea(ring)), 1e-9)
			for _, poly := range polys {
				min, max := lonRange(poly[0])
				if min < -180 || max > 180 {
					t.Errorf("part outside [-180,180]: %v..%v", min, max)
				}
			}
		})
	}
	// The east rectangle splits exactly at 180.
	polys, _, _ := NormalizePolygon([][]P{rect(170, 0, 190, 10)})
	var spans [][2]float64
	for _, p := range polys {
		a, b := lonRange(p[0])
		spans = append(spans, [2]float64{a, b})
	}
	has := func(a, b float64) bool {
		for _, s := range spans {
			if math.Abs(s[0]-a) < 1e-12 && math.Abs(s[1]-b) < 1e-12 {
				return true
			}
		}
		return false
	}
	if !has(170, 180) || !has(-180, -170) {
		t.Errorf("spans = %v, want [170,180] and [-180,-170]", spans)
	}
}

func TestNormalizeRefusals(t *testing.T) {
	if _, _, err := NormalizePolygon(nil); err == nil {
		t.Error("no rings")
	}
	if _, _, err := NormalizePolygon([][]P{rect(-10, 0, 400, 10)}); !errors.Is(err, ErrTooWide) {
		t.Errorf("too wide: %v", err)
	}
	if _, _, err := NormalizePolygon([][]P{rect(170, 0, 190, 10), rect(175, 2, 177, 4)}); !errors.Is(err, ErrAntimeridianHoles) {
		t.Errorf("hole across antimeridian: %v", err)
	}
	// A ring that straddles both +180 and -180 is not a real area on one sheet.
	if _, _, err := NormalizePolygon([][]P{{{-190, 0}, {190, 0}, {190, 10}, {-190, 10}, {-190, 0}}}); !errors.Is(err, ErrTooWide) {
		t.Errorf("both sides: %v", err)
	}
}

func TestPolygonsBuildsMultiPolygon(t *testing.T) {
	g, steps, err := Polygons([][][]P{{rect(0, 0, 1, 1)}, {reversed(rect(5, 5, 6, 6))}})
	if err != nil || g.Type != model.GeomMultiPolygon || len(g.Polygons) != 2 || len(steps) != 1 {
		t.Fatalf("g=%s polys=%d steps=%v err=%v", g.Type, len(g.Polygons), steps, err)
	}
	g, _, err = Polygons([][][]P{{rect(0, 0, 1, 1)}})
	if err != nil || g.Type != model.GeomPolygon {
		t.Fatalf("single polygon: %v %v", g.Type, err)
	}
	if _, _, err := Polygons([][][]P{{rect(0, 0, 1, 1)}, {}}); err == nil {
		t.Error("an empty polygon must fail the whole geometry, not be skipped")
	}
}

// A collapsed part (live example: AWC SBGL 95 had a part of two distinct
// points) has no area, so dropping it loses nothing, and the drop is recorded.
// A real part is never dropped.
func TestPolygonsDropsOnlyZeroAreaParts(t *testing.T) {
	sliver := []P{{-48.367, -22.85}, {-48.367, -22.85}, {-47.567, -23.117}, {-48.367, -22.85}}
	g, steps, err := Polygons([][][]P{{rect(0, 0, 1, 1)}, {sliver}})
	if err != nil || g.Type != model.GeomPolygon || len(g.Polygons) != 1 {
		t.Fatalf("g=%s polys=%d err=%v", g.Type, len(g.Polygons), err)
	}
	if len(steps) != 1 || steps[0] != StepDegenerateDrop {
		t.Errorf("the drop must be recorded: %v", steps)
	}
	// A thin but real part is kept.
	if g, steps, err := Polygons([][][]P{{rect(0, 0, 1, 1)}, {rect(5, 5, 5.000001, 6)}}); err != nil || len(g.Polygons) != 2 || len(steps) != 0 {
		t.Errorf("thin real part: polys=%d steps=%v err=%v", len(g.Polygons), steps, err)
	}
	// If nothing real is left, that is an error, not an empty area.
	if _, _, err := Polygons([][][]P{{sliver}, {sliver}}); err == nil {
		t.Error("all-degenerate must fail")
	}
	// A single degenerate polygon is not "dropped": it fails validation.
	if _, _, err := Polygons([][][]P{{sliver}}); err == nil {
		t.Error("a lone degenerate polygon must fail")
	}
}
