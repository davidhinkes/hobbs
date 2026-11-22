package model

import (
	"testing"
	"time"
)

func validAdvisory() Advisory {
	return Advisory{ID: "KKCI 43E", Kind: KindSIGMET, Source: "awc.airsigmet", RawText: "x", RawFormat: RawOfficialText,
		Lower: Unknown(), Upper: FL(370), ValidFrom: tp(t0), ValidTo: tp(t1)}
}

func TestAdvisoryValidate(t *testing.T) {
	if err := validAdvisory().Validate(); err != nil {
		t.Fatal(err)
	}
	ring := []LatLon{{1, 1}, {1, 2}, {2, 2}, {1, 1}}
	r := 5.0
	c := &LatLon{1, 1}
	ok := map[string]func(*Advisory){
		"ring":              func(a *Advisory) { a.Geometry = &Geometry{Ring: ring} },
		"rings":             func(a *Advisory) { a.Geometry = &Geometry{Rings: [][]LatLon{ring, ring}} },
		"line":              func(a *Advisory) { a.Geometry = &Geometry{Line: ring[:2]} },
		"circle":            func(a *Advisory) { a.Geometry = &Geometry{Center: c, RadiusNM: &r} },
		"upstream json raw": func(a *Advisory) { a.RawFormat = RawUpstreamJSON },
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
		"no id":           func(a *Advisory) { a.ID = "" },
		"no kind":         func(a *Advisory) { a.Kind = "" },
		"no source":       func(a *Advisory) { a.Source = "" },
		"no raw":          func(a *Advisory) { a.RawText = "" },
		"bad raw format":  func(a *Advisory) { a.RawFormat = "" },
		"window reversed": func(a *Advisory) { a.ValidTo = tp(t0.Add(-time.Hour)) },
		"zero-value alt":  func(a *Advisory) { a.Upper = Altitude{} },
		"empty geometry":  func(a *Advisory) { a.Geometry = &Geometry{} },
		"two shapes":      func(a *Advisory) { a.Geometry = &Geometry{Ring: ring, Line: ring[:2]} },
		"ring and rings":  func(a *Advisory) { a.Geometry = &Geometry{Ring: ring, Rings: [][]LatLon{ring}} },
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

func TestAdvisoryActiveAt(t *testing.T) {
	a := validAdvisory()
	for _, tt := range []struct {
		at     time.Time
		active bool
	}{{t0.Add(-time.Second), false}, {t0, true}, {t1.Add(-time.Second), true}, {t1, false}} {
		if got, det := a.ActiveAt(tt.at); got != tt.active || !det {
			t.Errorf("ActiveAt(%v) = (%v,%v), want (%v,true)", tt.at, got, det, tt.active)
		}
	}
	a.ValidTo = nil
	if got, det := a.ActiveAt(t0); !got || det {
		t.Errorf("missing bound must be possibly active, not determinate: (%v,%v)", got, det)
	}
}
