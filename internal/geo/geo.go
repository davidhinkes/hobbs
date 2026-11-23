// Package geo translates the many ways sources describe areas into the one
// canonical representation (RFC 7946 GeoJSON geometry, see model.Geometry).
//
// It does translation only. There is deliberately no intersection, containment
// or "does this apply" logic here or anywhere in the server: whether a shape
// matters to a flight is for the layers above to decide. Everything in this
// package is deterministic and covered by tests.
package geo

import (
	"errors"
	"fmt"
	"math"

	"github.com/davidhinkes/hobbs/internal/model"
)

// Step names recorded in model.Derivation.Steps.
const (
	StepReoriented        = "ring_reoriented"
	StepAntimeridianShift = "antimeridian_shift"
	StepAntimeridianCut   = "antimeridian_cut"
	// StepDegenerateDrop: a part with zero area (collapsed to a point or a line)
	// was dropped from a multi-part area. It has no region to lose; a part that
	// is not degenerate is never dropped.
	StepDegenerateDrop = "degenerate_part_dropped"
)

var (
	// ErrAntimeridianHoles: a polygon with holes that crosses the antimeridian.
	ErrAntimeridianHoles = errors.New("polygon with holes crosses the antimeridian; not supported")
	// ErrTooWide: a ring spans more than 360 degrees of longitude.
	ErrTooWide = errors.New("ring spans more than 360 degrees of longitude")
)

// Orient returns the rings wound per RFC 7946: exterior counter-clockwise,
// holes clockwise. changed reports whether any ring was reversed.
func Orient(rings [][]model.Position) (out [][]model.Position, changed bool) {
	out = make([][]model.Position, len(rings))
	for i, r := range rings {
		area := model.SignedArea(r)
		wantCCW := i == 0
		if (area > 0) != wantCCW && area != 0 {
			r = reversed(r)
			changed = true
		}
		out[i] = r
	}
	return out, changed
}

func reversed(r []model.Position) []model.Position {
	o := make([]model.Position, len(r))
	for i := range r {
		o[len(r)-1-i] = r[i]
	}
	return o
}

func lonRange(r []model.Position) (min, max float64) {
	min, max = math.Inf(1), math.Inf(-1)
	for _, p := range r {
		min, max = math.Min(min, p[0]), math.Max(max, p[0])
	}
	return
}

func shift(rings [][]model.Position, dLon float64) [][]model.Position {
	out := make([][]model.Position, len(rings))
	for i, r := range rings {
		o := make([]model.Position, len(r))
		for j, p := range r {
			o[j] = model.Position{p[0] + dLon, p[1]}
		}
		out[i] = o
	}
	return out
}

// clipLon clips a closed ring against the vertical line lon = limit
// (Sutherland-Hodgman), keeping the side where lon <= limit (keepLE) or
// lon >= limit. It returns nil when nothing of positive area remains.
func clipLon(ring []model.Position, limit float64, keepLE bool) []model.Position {
	pts := ring[:len(ring)-1] // open form
	inside := func(p model.Position) bool {
		if keepLE {
			return p[0] <= limit
		}
		return p[0] >= limit
	}
	cross := func(a, b model.Position) model.Position {
		t := (limit - a[0]) / (b[0] - a[0])
		return model.Position{limit, a[1] + t*(b[1]-a[1])}
	}
	var out []model.Position
	for i := range pts {
		cur, next := pts[i], pts[(i+1)%len(pts)]
		switch ci, ni := inside(cur), inside(next); {
		case ci && ni:
			out = append(out, next)
		case ci && !ni:
			out = append(out, cross(cur, next))
		case !ci && ni:
			out = append(out, cross(cur, next), next)
		}
	}
	// Drop consecutive duplicates, then close.
	var dedup []model.Position
	for _, p := range out {
		if len(dedup) == 0 || dedup[len(dedup)-1] != p {
			dedup = append(dedup, p)
		}
	}
	if len(dedup) > 1 && dedup[0] == dedup[len(dedup)-1] {
		dedup = dedup[:len(dedup)-1]
	}
	if len(dedup) < 3 {
		return nil
	}
	closed := append(dedup, dedup[0])
	if math.Abs(model.SignedArea(closed)) < 1e-12 {
		return nil
	}
	return closed
}

// NormalizePolygon translates one polygon (exterior ring first, then holes;
// closed rings; longitudes "unwrapped", i.e. continuous and possibly beyond
// +/-180) into one or more canonical polygons with longitudes in
// [-180, 180]: a ring wholly beyond the antimeridian is shifted by 360
// degrees, and a ring straddling it is cut at +/-180 (RFC 7946 section
// 3.1.9). Rings are wound per RFC 7946. steps lists what was applied.
func NormalizePolygon(rings [][]model.Position) (polys [][][]model.Position, steps []string, err error) {
	if len(rings) == 0 {
		return nil, nil, errors.New("polygon has no rings")
	}
	ext := rings[0]
	min, max := lonRange(ext)
	if max-min > 360 {
		return nil, nil, ErrTooWide
	}
	oriented := func(rs [][]model.Position) [][]model.Position {
		o, changed := Orient(rs)
		if changed {
			steps = appendOnce(steps, StepReoriented)
		}
		return o
	}
	switch {
	case min >= -180 && max <= 180:
		return [][][]model.Position{oriented(rings)}, steps, nil
	case min >= 180:
		steps = appendOnce(steps, StepAntimeridianShift)
		return [][][]model.Position{oriented(shift(rings, -360))}, steps, nil
	case max <= -180:
		steps = appendOnce(steps, StepAntimeridianShift)
		return [][][]model.Position{oriented(shift(rings, 360))}, steps, nil
	}
	if len(rings) > 1 {
		return nil, nil, ErrAntimeridianHoles
	}
	var a, b []model.Position
	switch {
	case max > 180 && min >= -180:
		a = clipLon(ext, 180, true)
		if b = clipLon(ext, 180, false); b != nil {
			b = shift([][]model.Position{b}, -360)[0]
		}
	case min < -180 && max <= 180:
		a = clipLon(ext, -180, false)
		if b = clipLon(ext, -180, true); b != nil {
			b = shift([][]model.Position{b}, 360)[0]
		}
	default:
		return nil, nil, ErrTooWide
	}
	steps = appendOnce(steps, StepAntimeridianCut)
	for _, part := range [][]model.Position{a, b} {
		if part != nil {
			polys = append(polys, oriented([][]model.Position{part}))
		}
	}
	if len(polys) == 0 {
		return nil, nil, errors.New("antimeridian cut left no area")
	}
	return polys, steps, nil
}

func appendOnce(s []string, v string) []string {
	for _, x := range s {
		if x == v {
			return s
		}
	}
	return append(s, v)
}

// Polygons builds a canonical Geometry from several input polygons, each an
// exterior ring plus holes in unwrapped longitudes. Every polygon is
// normalized; the result is a Polygon or MultiPolygon.
func Polygons(in [][][]model.Position) (model.Geometry, []string, error) {
	var all [][][]model.Position
	var steps []string
	for i, p := range in {
		if len(in) > 1 && len(p) > 0 && model.SignedArea(p[0]) == 0 && len(p[0]) >= 4 {
			steps = appendOnce(steps, StepDegenerateDrop)
			continue
		}
		out, st, err := NormalizePolygon(p)
		if err != nil {
			return model.Geometry{}, nil, fmt.Errorf("polygon %d: %w", i, err)
		}
		all = append(all, out...)
		for _, s := range st {
			steps = appendOnce(steps, s)
		}
	}
	if len(all) == 0 {
		return model.Geometry{}, nil, errors.New("every part is degenerate (zero area)")
	}
	g := model.NewPolygons(all)
	if err := g.Validate(); err != nil {
		return model.Geometry{}, nil, err
	}
	return g, steps, nil
}
