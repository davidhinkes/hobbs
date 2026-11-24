package model

import (
	"testing"
	"time"
)

func validAdvisory() Advisory {
	return Advisory{ID: "KKCI 43E", Kind: KindSIGMET, Source: "awc.airsigmet", RawText: "x", RawFormat: RawAdvisoryText,
		Lower: Unknown(), Upper: FL(370), ValidFrom: tp(t0), ValidTo: tp(t1)}
}

func TestAdvisoryValidate(t *testing.T) {
	if err := validAdvisory().Validate(); err != nil {
		t.Fatal(err)
	}
	ccw := [][]Position{{{0, 0}, {1, 0}, {1, 1}, {0, 1}, {0, 0}}}
	ok := map[string]func(*Advisory){
		"point":   func(a *Advisory) { g := NewPoint(-77.4, 38.9); a.Geometry = &g },
		"line":    func(a *Advisory) { g := NewLineString([]Position{{0, 0}, {1, 1}}); a.Geometry = &g },
		"polygon": func(a *Advisory) { g := NewPolygons([][][]Position{ccw[0:1]}); a.Geometry = &g },
		"multipolygon": func(a *Advisory) {
			g := NewPolygons([][][]Position{ccw, {{{5, 5}, {6, 5}, {6, 6}, {5, 6}, {5, 5}}}})
			a.Geometry = &g
		},
		"source record only": func(a *Advisory) {
			a.RawText, a.RawFormat, a.SourceRecord = "", "", []byte(`{"hazard":"ICE"}`)
		},
		"text and source record": func(a *Advisory) { a.SourceRecord = []byte(`{"x":1}`) },
	}
	for name, mut := range ok {
		t.Run("ok "+name, func(t *testing.T) {
			a := validAdvisory()
			mut(&a)
			if err := a.Validate(); err != nil {
				t.Error(err)
			}
		})
	}
	bad := map[string]func(*Advisory){
		"no id":              func(a *Advisory) { a.ID = "" },
		"no kind":            func(a *Advisory) { a.Kind = "" },
		"no source":          func(a *Advisory) { a.Source = "" },
		"no text, no record": func(a *Advisory) { a.RawText, a.RawFormat = "", "" },
		"format, no text":    func(a *Advisory) { a.RawText, a.SourceRecord = "", []byte(`{}`) },
		"text, no format":    func(a *Advisory) { a.RawFormat = "" },
		"notam format":       func(a *Advisory) { a.RawFormat = RawICAONotam },
		"invalid record": func(a *Advisory) {
			a.RawText, a.RawFormat, a.SourceRecord = "", "", []byte(`{not json`)
		},
		"window reversed": func(a *Advisory) { a.ValidTo = tp(t0.Add(-time.Hour)) },
		"zero-value alt":  func(a *Advisory) { a.Upper = Altitude{} },
		"empty geometry":  func(a *Advisory) { a.Geometry = &Geometry{} },
		"clockwise exterior": func(a *Advisory) {
			g := Geometry{Type: GeomPolygon, Polygons: [][][]Position{{{{0, 0}, {0, 1}, {1, 1}, {1, 0}, {0, 0}}}}}
			a.Geometry = &g
		},
		"lon out of range": func(a *Advisory) { g := NewPoint(181, 0); a.Geometry = &g },
		"open ring": func(a *Advisory) {
			g := Geometry{Type: GeomPolygon, Polygons: [][][]Position{{{{0, 0}, {1, 0}, {1, 1}, {0, 1}}}}}
			a.Geometry = &g
		},
	}
	for name, mut := range bad {
		t.Run("bad "+name, func(t *testing.T) {
			a := validAdvisory()
			mut(&a)
			if a.Validate() == nil {
				t.Error("expected error")
			}
		})
	}
}
