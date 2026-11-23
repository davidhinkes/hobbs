package provenance

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

var now = time.Date(2026, 11, 22, 17, 30, 0, 0, time.UTC)

func src(id string) Source {
	ts := now.Add(-5 * time.Minute)
	return Source{
		ID: id, Authority: "NOAA/NWS Aviation Weather Center",
		URL:           "https://aviationweather.gov/api/data/gairmet?format=json",
		Documentation: Documented, RetrievedAt: now.Add(-48 * time.Second),
		UpstreamTimestamp: &ts, ServedFrom: Live, HTTPStatus: 200,
		RecordsInFeed: 34, RecordsReturned: 3,
	}
}

func TestNewDerivesStatus(t *testing.T) {
	stale := src("awc.stale")
	stale.ServedFrom = StaleCache
	undoc := src("faa.tfr")
	undoc.Documentation = Undocumented

	tests := []struct {
		name    string
		sources []Source
		nc      []NotConsulted
		issues  []Issue
		want    Status
		none    bool
	}{
		{"clean", []Source{src("awc.gairmet")}, nil, nil, Complete, true},
		{"undocumented is info only", []Source{undoc}, nil, nil, Complete, true},
		{"warning degrades", []Source{src("a")}, nil,
			[]Issue{{Code: GeometryUnavailable, Severity: Warning, Message: "12 text-only TFRs"}}, Degraded, false},
		{"stale cache degrades", []Source{stale}, nil, nil, Degraded, false},
		{"critical is incomplete", []Source{src("a")}, nil,
			[]Issue{{Code: SourceUnavailable, Severity: Critical, Source: "a", Message: "down"}}, Incomplete, false},
		{"not consulted is incomplete", []Source{src("a")}, []NotConsulted{{ID: "faa.nms.notam", Reason: "not_enabled"}}, nil, Incomplete, false},
		{"only not consulted is allowed", nil, []NotConsulted{{ID: "faa.nms.notam", Reason: "not_enabled"}}, nil, Incomplete, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env, err := New("x", now, tt.sources, tt.nc, tt.issues)
			if err != nil {
				t.Fatal(err)
			}
			if env.Completeness.Status != tt.want {
				t.Errorf("status = %s, want %s", env.Completeness.Status, tt.want)
			}
			if env.Completeness.CanAssertNone() != tt.none {
				t.Errorf("CanAssertNone = %v, want %v", env.Completeness.CanAssertNone(), tt.none)
			}
			if env.Disclaimer != Disclaimer {
				t.Error("disclaimer missing")
			}
		})
	}
}

func TestNewAddsImpliedIssues(t *testing.T) {
	stale := src("awc.gairmet")
	stale.ServedFrom = StaleCache
	undoc := src("faa.tfr")
	undoc.Documentation = Undocumented
	skewed := src("awc.cwa")
	future := now.Add(time.Hour)
	skewed.UpstreamTimestamp = &future

	env, err := New("x", now, []Source{stale, undoc, skewed}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := map[Code]string{}
	for _, is := range env.Completeness.Issues {
		got[is.Code] = is.Source
	}
	for code, srcID := range map[Code]string{StaleData: "awc.gairmet", UndocumentedSource: "faa.tfr", ClockSkew: "awc.cwa"} {
		if got[code] != srcID {
			t.Errorf("missing %s for %s; issues=%v", code, srcID, env.Completeness.Issues)
		}
	}
}

func TestNewDoesNotDuplicateExplicitIssue(t *testing.T) {
	stale := src("awc.gairmet")
	stale.ServedFrom = StaleCache
	env, err := New("x", now, []Source{stale}, nil,
		[]Issue{{Code: StaleData, Severity: Warning, Source: "awc.gairmet", Message: "custom"}})
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, is := range env.Completeness.Issues {
		if is.Code == StaleData {
			n++
		}
	}
	if n != 1 {
		t.Errorf("STALE_DATA count = %d, want 1", n)
	}
}

func TestNewComputesAges(t *testing.T) {
	env, err := New("x", now, []Source{src("a")}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	s := env.Provenance.Sources[0]
	if s.AgeSeconds != 48 {
		t.Errorf("age = %d, want 48", s.AgeSeconds)
	}
	if s.UpstreamAgeSeconds == nil || *s.UpstreamAgeSeconds != 300 {
		t.Errorf("upstream age = %v, want 300", s.UpstreamAgeSeconds)
	}
}

func TestNewRejectsBadProvenance(t *testing.T) {
	noURL := src("a")
	noURL.URL = ""
	noTime := src("a")
	noTime.RetrievedAt = time.Time{}
	badDoc := src("a")
	badDoc.Documentation = "maybe"
	badFrom := src("a")
	badFrom.ServedFrom = "memory"

	tests := []struct {
		name    string
		now     time.Time
		sources []Source
		nc      []NotConsulted
		issues  []Issue
	}{
		{"no provenance at all", now, nil, nil, nil},
		{"zero evaluatedAt", time.Time{}, []Source{src("a")}, nil, nil},
		{"missing url", now, []Source{noURL}, nil, nil},
		{"missing retrieved_at", now, []Source{noTime}, nil, nil},
		{"bad documentation", now, []Source{badDoc}, nil, nil},
		{"bad served_from", now, []Source{badFrom}, nil, nil},
		{"duplicate source", now, []Source{src("a"), src("a")}, nil, nil},
		{"unknown code", now, []Source{src("a")}, nil, []Issue{{Code: "NOPE", Severity: Info, Message: "m"}}},
		{"bad severity", now, []Source{src("a")}, nil, []Issue{{Code: StaleData, Severity: "high", Message: "m"}}},
		{"empty message", now, []Source{src("a")}, nil, []Issue{{Code: StaleData, Severity: Info}}},
		{"bad not-consulted", now, nil, []NotConsulted{{ID: "x"}}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := New("x", tt.now, tt.sources, tt.nc, tt.issues); err == nil {
				t.Error("expected error")
			}
		})
	}
}

// TestJSONContract pins the wire format agents depend on: every envelope must
// serialize provenance and completeness, even when data is empty.
func TestJSONContract(t *testing.T) {
	env, err := New([]string{}, now, []Source{src("awc.gairmet")}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"data", "evaluated_at", "provenance", "completeness", "airworthy", "disclaimer"} {
		if _, ok := m[k]; !ok {
			t.Errorf("missing top-level key %q", k)
		}
	}
	p := m["provenance"].(map[string]any)
	for _, k := range []string{"sources", "not_consulted"} {
		if _, ok := p[k]; !ok {
			t.Errorf("provenance missing %q", k)
		}
	}
	c := m["completeness"].(map[string]any)
	if c["status"] != "complete" {
		t.Errorf("status = %v", c["status"])
	}
	if c["issues"] == nil {
		t.Error("issues must serialize as [] not null")
	}
	if p["not_consulted"] == nil {
		t.Error("not_consulted must serialize as [] not null")
	}
	s := p["sources"].([]any)[0].(map[string]any)
	for _, k := range []string{"id", "authority", "url", "documentation", "retrieved_at", "age_seconds", "served_from", "records_in_feed", "records_returned"} {
		if _, ok := s[k]; !ok {
			t.Errorf("source missing %q", k)
		}
	}
	if !strings.HasSuffix(m["evaluated_at"].(string), "Z") {
		t.Errorf("evaluated_at not UTC: %v", m["evaluated_at"])
	}
}

func TestIssuesSortedBySeverity(t *testing.T) {
	env, err := New("x", now, []Source{src("a")}, nil, []Issue{
		{Code: GeometryPartial, Severity: Info, Message: "i"},
		{Code: SourceUnavailable, Severity: Critical, Message: "c"},
		{Code: StaleData, Severity: Warning, Message: "w"},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := []Severity{}
	for _, is := range env.Completeness.Issues {
		got = append(got, is.Severity)
	}
	want := []Severity{Critical, Warning, Info}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}

// Airworthy is part of the wire contract and defaults to false: an envelope
// nobody explicitly declared airworthy is not.
func TestAirworthyDefaultsToFalse(t *testing.T) {
	env, err := New("x", now, []Source{src("a")}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(env)
	if !strings.Contains(string(b), `"airworthy":false`) {
		t.Errorf("airworthy must be serialized and false by default: %s", b)
	}
}
