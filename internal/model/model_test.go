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
	return Notam{ID: "FDC 6/9306", RawText: "!FDC 6/9306 ...", RawFormat: RawOfficialText, Lower: AGL(0), Upper: MSL(4000),
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

func TestOthers(t *testing.T) {
	all := []Translation{{Format: "LOCAL_FORMAT", Text: "L"}, {Format: "ICAO", Text: "I"}}
	if got := Others(all, 1); len(got) != 1 || got[0].Format != "LOCAL_FORMAT" {
		t.Errorf("Others(all, 1) = %v", got)
	}
	if got := Others(all[:1], 0); got != nil {
		t.Errorf("Others of one = %v, want nil", got)
	}
	if len(all) != 2 || all[1].Format != "ICAO" {
		t.Errorf("Others modified its input: %v", all)
	}
}
