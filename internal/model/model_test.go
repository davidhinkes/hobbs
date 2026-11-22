package model

import (
	"encoding/json"
	"testing"
	"time"
)

func tp(t time.Time) *time.Time { return &t }

var (
	t0 = time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	t1 = t0.Add(7 * 24 * time.Hour)
)

func TestAltitudeValidate(t *testing.T) {
	ok := []Altitude{MSL(4000), AGL(0), FL(370), SFC(), Unlimited(), Unknown()}
	for _, a := range ok {
		if err := a.Validate(); err != nil {
			t.Errorf("%+v: %v", a, err)
		}
	}
	zero := 0
	bad := []Altitude{{Datum: DatumMSL}, {Datum: DatumSFC, Value: &zero}, {Datum: "X"}, {}}
	for _, a := range bad {
		if a.Validate() == nil {
			t.Errorf("%+v: expected error", a)
		}
	}
}

// Unknown must never be confused with zero.
func TestUnknownAltitudeIsNotZero(t *testing.T) {
	u := Unknown()
	if u.Known() || u.Value != nil {
		t.Fatalf("unknown altitude must be unknown with nil value: %+v", u)
	}
	b, _ := json.Marshal(u)
	if string(b) != `{"datum":"UNKNOWN"}` {
		t.Errorf("json = %s", b)
	}
	if !AGL(0).Known() {
		t.Error("AGL 0 is a real value")
	}
}

func valid() Notam {
	return Notam{ID: "FDC 6/9306", RawText: "!FDC 6/9306 ...", Lower: AGL(0), Upper: MSL(4000),
		EffectiveFrom: tp(t0), EffectiveTo: tp(t1)}
}

func TestNotamValidate(t *testing.T) {
	if err := valid().Validate(); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(*Notam){
		"no id":             func(n *Notam) { n.ID = "" },
		"no raw text":       func(n *Notam) { n.RawText = "" },
		"to before from":    func(n *Notam) { n.EffectiveTo = tp(t0.Add(-time.Hour)) },
		"permanent with to": func(n *Notam) { n.Permanent = true },
		"bad altitude":      func(n *Notam) { n.Lower = Altitude{Datum: DatumMSL} },
		"zero-value alt":    func(n *Notam) { n.Upper = Altitude{} },
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			n := valid()
			mut(&n)
			if n.Validate() == nil {
				t.Error("expected error")
			}
		})
	}
}

func TestActiveAt(t *testing.T) {
	tests := []struct {
		name       string
		mut        func(*Notam)
		at         time.Time
		wantActive bool
		wantDet    bool
	}{
		{"inside window", func(*Notam) {}, t0.Add(time.Hour), true, true},
		{"start is inclusive", func(*Notam) {}, t0, true, true},
		{"end is exclusive", func(*Notam) {}, t1, false, true},
		{"before start", func(*Notam) {}, t0.Add(-time.Second), false, true},
		{"permanent", func(n *Notam) { n.EffectiveTo = nil; n.Permanent = true }, t1.Add(1000 * time.Hour), true, true},
		{"open-ended unstated end", func(n *Notam) { n.EffectiveTo = nil }, t1, true, false},
		{"no start time", func(n *Notam) { n.EffectiveFrom = nil }, t0, true, false},
		{"scheduled", func(n *Notam) { n.Schedule = "DAILY 1200-2300" }, t0.Add(-time.Hour), true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n := valid()
			tt.mut(&n)
			a, d := n.ActiveAt(tt.at)
			if a != tt.wantActive || d != tt.wantDet {
				t.Errorf("ActiveAt = (%v,%v), want (%v,%v)", a, d, tt.wantActive, tt.wantDet)
			}
		})
	}
}
