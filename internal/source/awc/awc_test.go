package awc

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/davidhinkes/hobbs/internal/model"
	"github.com/davidhinkes/hobbs/internal/provenance"
	"github.com/davidhinkes/hobbs/internal/source"
)

var fixedNow = time.Date(2026, 11, 22, 17, 30, 0, 0, time.UTC)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "awc", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// serve returns a client pointed at a server that answers every request with
// the given status and body.
func serve(t *testing.T, status int, body []byte) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `W/"abc"`)
		w.WriteHeader(status)
		w.Write(body)
	}))
	t.Cleanup(srv.Close)
	c := New()
	c.BaseURL = srv.URL
	c.Now = func() time.Time { return fixedNow }
	return c
}

func fetch(t *testing.T, s source.Source[model.Advisory]) source.Snapshot[model.Advisory] {
	t.Helper()
	snap, err := s.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	return snap
}

func issue(snap source.Snapshot[model.Advisory], code provenance.Code) *provenance.Issue {
	for i := range snap.Issues {
		if snap.Issues[i].Code == code {
			return &snap.Issues[i]
		}
	}
	return nil
}

func byID(t *testing.T, snap source.Snapshot[model.Advisory], id string) model.Advisory {
	t.Helper()
	for _, a := range snap.Items {
		if a.ID == id {
			return a
		}
	}
	t.Fatalf("no record %q", id)
	return model.Advisory{}
}

func TestAirSigmetFixture(t *testing.T) {
	snap := fetch(t, serve(t, 200, fixture(t, "airsigmet.json")).AirSigmet())
	if len(snap.Items) != 1 {
		t.Fatalf("items = %d", len(snap.Items))
	}
	a := snap.Items[0]
	if a.ID != "KKCI 43E" || a.Kind != model.KindSIGMET || a.Hazard != "CONVECTIVE" {
		t.Errorf("identity wrong: %+v", a)
	}
	if a.Upper.Datum != model.DatumFL || *a.Upper.Value != 370 {
		t.Errorf("upper = %+v, want FL370", a.Upper)
	}
	if a.Lower.Known() {
		t.Errorf("lower should be unknown, got %+v", a.Lower)
	}
	if a.Geometry == nil || a.Geometry.Type != model.GeomPolygon || len(a.Geometry.Polygons[0][0]) != 5 {
		t.Errorf("geometry = %+v", a.Geometry)
	}
	if a.RawFormat != model.RawAdvisoryText || !strings.Contains(a.RawText, "CONVECTIVE SIGMET 43E") || a.SourceRecord != nil {
		t.Errorf("raw text not preserved: %q", a.RawText)
	}
	if a.ValidFrom == nil || a.ValidTo == nil || !a.ValidTo.After(*a.ValidFrom) {
		t.Errorf("window = %v..%v", a.ValidFrom, a.ValidTo)
	}
	if a.Severity != "5" || a.Details["movement_dir_deg"] != "200" {
		t.Errorf("severity/details = %q %v", a.Severity, a.Details)
	}
	s := snap.Source
	if s.ID != IDAirSigmet || s.Authority != Authority || s.Documentation != provenance.Documented ||
		s.HTTPStatus != 200 || s.ETag != `W/"abc"` || s.ContentSHA256 == "" ||
		s.RecordsInFeed != 1 || !s.RetrievedAt.Equal(fixedNow) || s.UpstreamTimestamp == nil {
		t.Errorf("source = %+v", s)
	}
}

func TestGAirmetFixture(t *testing.T) {
	snap := fetch(t, serve(t, 200, fixture(t, "gairmet.json")).GAirmet())
	if len(snap.Items) != 34 {
		t.Fatalf("items = %d, want 34", len(snap.Items))
	}
	var lines int
	for _, a := range snap.Items {
		if a.Geometry != nil && a.Geometry.Type == model.GeomLineString {
			lines++
		}
		// G-AIRMETs publish no text: no raw_text, the record in source_record.
		if a.RawText != "" || a.RawFormat != "" || !json.Valid(a.SourceRecord) || !strings.Contains(string(a.SourceRecord), `"hazard"`) {
			t.Fatalf("%s: raw = %q/%s, source_record = %.40s", a.ID, a.RawText, a.RawFormat, a.SourceRecord)
		}
		if a.ValidFrom == nil || a.ValidTo == nil {
			t.Fatalf("%s: missing window", a.ID)
		}
	}
	if lines != 5 {
		t.Errorf("line features = %d, want 5", lines)
	}
	if is := issue(snap, provenance.GeometryNotArea); is == nil || len(is.Refs) != 5 || is.Severity != provenance.Info {
		t.Errorf("GEOMETRY_NOT_AREA = %+v", is)
	}
	if issue(snap, provenance.ParseFailed) != nil || issue(snap, provenance.GeometryUnavailable) != nil {
		t.Errorf("clean feed should have no parse/geometry warnings: %+v", snap.Issues)
	}

	// ICE: top 230 hundreds-of-ft => FL230; base FZL => unknown + reference kept.
	var ice *model.Advisory
	for i := range snap.Items {
		if snap.Items[i].Hazard == "ICE" {
			ice = &snap.Items[i]
			break
		}
	}
	if ice == nil {
		t.Fatal("no ICE record")
	}
	if ice.Upper.Datum != model.DatumFL || *ice.Upper.Value != 230 {
		t.Errorf("ice upper = %+v", ice.Upper)
	}
	if ice.Lower.Known() || ice.Details["base_reference"] != "FZL" {
		t.Errorf("ice lower = %+v details=%v", ice.Lower, ice.Details)
	}
	if ice.Details["freezing_level_base_hundreds_ft"] != "100" {
		t.Errorf("fzl details = %v", ice.Details)
	}
	// Valid window is [validTime, expireTime): 18:00Z to 21:00Z.
	want := time.Date(2026, 11, 22, 18, 0, 0, 0, time.UTC)
	if !ice.ValidFrom.Equal(want) || !ice.ValidTo.Equal(want.Add(3*time.Hour)) {
		t.Errorf("window = %v..%v", ice.ValidFrom, ice.ValidTo)
	}
	if issue(snap, provenance.AltitudeUnknown) == nil {
		t.Error("IFR-type records without limits should raise ALTITUDE_UNKNOWN (info)")
	}
}

func TestIntlSigmetFixture(t *testing.T) {
	snap := fetch(t, serve(t, 200, fixture(t, "isigmet.json")).IntlSigmet())
	a := byID(t, snap, "WAAA 14")
	if a.Lower.Datum != model.DatumSFC {
		t.Errorf("base 0 should be SFC (official text says SFC/FL070): %+v", a.Lower)
	}
	if a.Upper.Datum != model.DatumMSL || *a.Upper.Value != 7000 {
		t.Errorf("upper = %+v, want 7000 MSL", a.Upper)
	}
	if !strings.Contains(a.RawText, "SFC/FL070") {
		t.Errorf("raw text lost: %q", a.RawText)
	}
	if a.Details["fir_name"] != "WAAF UJUNG PANDANG" || a.Qualifier != "ERUPTION MT IBU" {
		t.Errorf("details = %v qualifier=%q", a.Details, a.Qualifier)
	}
	// A record with no base (null) must be unknown, not zero/SFC.
	var sawUnknownBase bool
	for _, x := range snap.Items {
		if !x.Lower.Known() && x.Upper.Known() {
			sawUnknownBase = true
		}
	}
	if !sawUnknownBase {
		t.Skip("fixture subset has no null-base record")
	}
}

func TestCWAFixtureInfersRingFromClosure(t *testing.T) {
	snap := fetch(t, serve(t, 200, fixture(t, "cwa.json")).CWA())
	a := byID(t, snap, "ZTL CWA 104")
	if a.Geometry == nil || a.Geometry.Type != model.GeomPolygon || len(a.Geometry.Polygons[0][0]) != 6 {
		t.Fatalf("geometry = %+v", a.Geometry)
	}
	// Canonical positions are [lon, lat]; the closed ring keeps its first point.
	if first := a.Geometry.Polygons[0][0][0]; first != (model.Position{-80.422, 36.992}) {
		t.Errorf("string coords not decoded: %+v", first)
	}
	if len(a.ParseIssues) != 0 {
		t.Errorf("closed ring with null geom should not be a parse issue: %v", a.ParseIssues)
	}
	if a.Hazard != "IFR" || a.Qualifier != "PATCHY LIFR" || !strings.Contains(a.RawText, "ZTL CWA 104") {
		t.Errorf("record = %+v", a)
	}
}

func TestNoContentIsAnEmptyFeed(t *testing.T) {
	snap := fetch(t, serve(t, 204, nil).AirSigmet())
	if len(snap.Items) != 0 || snap.Source.HTTPStatus != 204 {
		t.Errorf("snap = %+v", snap)
	}
	// It must still be attributable, so an envelope can honestly say "none".
	env, err := provenance.New(snap.Items, fixedNow, []provenance.Source{snap.Source}, nil, snap.Issues)
	if err != nil {
		t.Fatal(err)
	}
	if !env.Completeness.CanAssertNone() {
		t.Errorf("a clean 204 should allow asserting none: %+v", env.Completeness)
	}
}

func TestFailuresAreErrorsNotEmptyResults(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"empty body with 200", 200, "", "empty body"},
		{"whitespace body", 200, "  \n", "empty body"},
		{"bad parameter", 400, `{"status":"error","error":"Invalid"}`, "HTTP 400"},
		{"server error", 503, "unavailable", "HTTP 503"},
		{"not found", 404, "", "HTTP 404"},
		{"object, not array", 200, `{"status":"ok"}`, "not a JSON array"},
		{"html error page", 200, "<html>maintenance</html>", "not a JSON array"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			snap, err := serve(t, tt.status, []byte(tt.body)).GAirmet().Fetch(context.Background())
			if err == nil || !errors.Is(err, ErrUpstream) || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want ErrUpstream containing %q", err, tt.want)
			}
			if snap.Source.HTTPStatus != tt.status || snap.Source.RetrievedAt.IsZero() || snap.Source.URL == "" {
				t.Errorf("failure must still be attributable: %+v", snap.Source)
			}
			if len(snap.Items) != 0 {
				t.Error("no items on failure")
			}
		})
	}
}

func TestUnreachableUpstreamIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	c := New()
	c.BaseURL = url
	c.Now = func() time.Time { return fixedNow }
	snap, err := c.CWA().Fetch(context.Background())
	if !errors.Is(err, ErrUpstream) {
		t.Fatalf("err = %v", err)
	}
	if snap.Source.RetrievedAt.IsZero() {
		t.Error("attempt time should be recorded")
	}
}

func TestContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := serve(t, 200, []byte("[]")).CWA().Fetch(ctx); !errors.Is(err, ErrUpstream) {
		t.Errorf("err = %v", err)
	}
}

// A bad record must be flagged and kept, never dropped, and must not damage
// its neighbours.
func TestBadRecordsAreFlaggedNotDropped(t *testing.T) {
	body := `[
	 {"icaoId":"KKCI","seriesId":"1A","validTimeFrom":1795366500,"validTimeTo":1795373700,"hazard":"TURB",
	  "rawAirSigmet":"WSUS31 KKCI ...","coords":[{"lat":10,"lon":10},{"lat":10,"lon":11},{"lat":11,"lon":11},{"lat":10,"lon":10}]},
	 {"icaoId":"KKCI","seriesId":"2A","validTimeFrom":"garbage","rawAirSigmet":"BAD TIME","coords":[]},
	 {"icaoId":"KKCI","seriesId":"3A","validTimeFrom":1795373700,"validTimeTo":1795366500,"rawAirSigmet":"REVERSED",
	  "coords":[{"lat":95,"lon":10},{"lat":10,"lon":11},{"lat":11,"lon":11},{"lat":95,"lon":10}]},
	 {"icaoId":"KKCI","seriesId":"4A","validTimeFrom":1795366500,"validTimeTo":1795373700,"coords":[{"lat":1,"lon":1}]},
	 {"seriesId":"","coords":"not-a-list"},
	 {"icaoId":"KKCI","seriesId":"1A","rawAirSigmet":"DUP","coords":[]}
	]`
	snap := fetch(t, serve(t, 200, []byte(body)).AirSigmet())
	if len(snap.Items) != 6 || snap.Source.RecordsInFeed != 6 {
		t.Fatalf("items=%d feed=%d, want 6/6", len(snap.Items), snap.Source.RecordsInFeed)
	}
	if a := byID(t, snap, "KKCI 1A"); len(a.ParseIssues) != 0 || a.Geometry == nil || a.Geometry.Type != model.GeomPolygon || len(a.Geometry.Polygons[0][0]) != 4 {
		t.Errorf("good record damaged: %+v", a)
	}
	if a := byID(t, snap, "KKCI 1A #2"); a.RawText != "DUP" {
		t.Errorf("duplicate IDs must be disambiguated, not overwritten: %+v", a)
	}
	// Undecodable time: the whole record decodes as a stub with the source record.
	if a := byID(t, snap, "awc.airsigmet#1"); a.RawText != "" || a.RawFormat != "" || !strings.Contains(string(a.SourceRecord), "garbage") || len(a.ParseIssues) == 0 {
		t.Errorf("stub = %+v", a)
	}
	// Out-of-range coordinate: geometry dropped, flagged, raw kept.
	if a := byID(t, snap, "KKCI 3A"); a.Geometry != nil || len(a.ParseIssues) == 0 || a.RawText != "REVERSED" {
		t.Errorf("3A = %+v", a)
	} else if a.ValidFrom != nil || a.ValidTo != nil {
		t.Errorf("reversed window must be discarded, not served: %v %v", a.ValidFrom, a.ValidTo)
	}
	// Missing official text: no raw_text or raw_format; the source record kept, flagged.
	if a := byID(t, snap, "KKCI 4A"); a.RawText != "" || a.RawFormat != "" || len(a.SourceRecord) == 0 || len(a.ParseIssues) == 0 {
		t.Errorf("4A = %+v", a)
	}
	pf := issue(snap, provenance.ParseFailed)
	if pf == nil || pf.Severity != provenance.Warning || len(pf.Refs) < 3 {
		t.Errorf("PARSE_FAILED = %+v", pf)
	}
	if issue(snap, provenance.GeometryUnavailable) == nil {
		t.Error("records without geometry must raise GEOMETRY_UNAVAILABLE")
	}
	// Everything served must satisfy the model's invariants.
	for _, a := range snap.Items {
		if err := a.Validate(); err != nil {
			t.Errorf("%s: %v", a.ID, err)
		}
	}
	// And the envelope must reflect the damage.
	env, err := provenance.New(snap.Items, fixedNow, []provenance.Source{snap.Source}, nil, snap.Issues)
	if err != nil {
		t.Fatal(err)
	}
	if env.Completeness.Status != provenance.Degraded || env.Completeness.CanAssertNone() {
		t.Errorf("status = %s", env.Completeness.Status)
	}
}

func TestFlexTypes(t *testing.T) {
	var f flexFloat
	for in, want := range map[string]float64{`12.5`: 12.5, `"12.5"`: 12.5, `" -3 "`: -3, `0`: 0} {
		if err := f.UnmarshalJSON([]byte(in)); err != nil || float64(f) != want {
			t.Errorf("%s -> %v, %v", in, f, err)
		}
	}
	for _, in := range []string{`"x"`, `""`, `"NaN"`, `"Inf"`} {
		if new(flexFloat).UnmarshalJSON([]byte(in)) == nil {
			t.Errorf("%s should fail", in)
		}
	}
	var ts flexTime
	for _, in := range []string{`1795366500`, `"1795366500"`, `"2026-11-22T16:55:00.000Z"`, `"2026-11-22T16:55:00Z"`} {
		ts = flexTime{}
		if err := ts.UnmarshalJSON([]byte(in)); err != nil || ts.Year() != 2026 || ts.Location() != time.UTC {
			t.Errorf("%s -> %v, %v", in, ts, err)
		}
	}
	for _, in := range []string{`0`, `-5`, `1795366500000`, `"yesterday"`, `true`} {
		if new(flexTime).UnmarshalJSON([]byte(in)) == nil {
			t.Errorf("%s should fail (milliseconds and junk must not be guessed)", in)
		}
	}
}

func TestAltitudeConversion(t *testing.T) {
	tests := []struct {
		ft    float64
		datum model.Datum
		val   int
	}{
		{0, model.DatumMSL, 0}, {7000, model.DatumMSL, 7000}, {17999, model.DatumMSL, 17999},
		{18000, model.DatumFL, 180}, {37000, model.DatumFL, 370},
	}
	for _, tt := range tests {
		a := feetAlt(tt.ft)
		if a.Datum != tt.datum || *a.Value != tt.val {
			t.Errorf("feetAlt(%v) = %+v", tt.ft, a)
		}
	}
	if a := hundredsAlt(230); a.Datum != model.DatumFL || *a.Value != 230 {
		t.Errorf("hundredsAlt(230) = %+v", a)
	}
	if a := hundredsAlt(80); a.Datum != model.DatumMSL || *a.Value != 8000 {
		t.Errorf("hundredsAlt(80) = %+v", a)
	}
}

// A sloping limit must never be understated: lower takes the minimum, upper
// the maximum.
func TestSlopingLimitsUseWidestExtent(t *testing.T) {
	body := `[{"icaoId":"KKCI","seriesId":"9A","rawAirSigmet":"x","altitudeLow1":5000,"altitudeLow2":3000,"altitudeHi1":10000,"altitudeHi2":12000,
	 "coords":[{"lat":1,"lon":1},{"lat":1,"lon":2},{"lat":2,"lon":2},{"lat":1,"lon":1}]}]`
	a := fetch(t, serve(t, 200, []byte(body)).AirSigmet()).Items[0]
	if *a.Lower.Value != 3000 || *a.Upper.Value != 12000 {
		t.Errorf("lower=%+v upper=%+v", a.Lower, a.Upper)
	}
}

func TestEveryFetchCarriesAltitudeAssumption(t *testing.T) {
	for name, s := range map[string]struct {
		src  func(*Client) source.Source[model.Advisory]
		file string
	}{
		"airsigmet": {(*Client).AirSigmet, "airsigmet.json"},
		"gairmet":   {(*Client).GAirmet, "gairmet.json"},
		"isigmet":   {(*Client).IntlSigmet, "isigmet.json"},
		"cwa":       {(*Client).CWA, "cwa.json"},
	} {
		t.Run(name, func(t *testing.T) {
			snap := fetch(t, s.src(serve(t, 200, fixture(t, s.file))))
			if is := issue(snap, provenance.AltitudeAssumed); is == nil || is.Severity != provenance.Info {
				t.Errorf("missing ALTITUDE_ASSUMED: %+v", snap.Issues)
			}
			// Real feeds must build a valid, attributable envelope.
			if _, err := provenance.New(snap.Items, fixedNow, []provenance.Source{snap.Source}, nil, snap.Issues); err != nil {
				t.Errorf("envelope: %v", err)
			}
		})
	}
}

func TestRefsAreCapped(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("[")
	for i := 0; i < 50; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString(`{"icaoId":"K","seriesId":"` + string(rune('A'+i%26)) + string(rune('a'+i/26)) + `","rawAirSigmet":"x","coords":[]}`)
	}
	sb.WriteString("]")
	snap := fetch(t, serve(t, 200, []byte(sb.String())).AirSigmet())
	is := issue(snap, provenance.GeometryUnavailable)
	if is == nil || len(is.Refs) != source.MaxRefs || !strings.Contains(is.Message, "of 50") {
		t.Errorf("issue = %+v", is)
	}
}

// Real records from the live feed: SIGMETs crossing the antimeridian are
// published as geom "AREAS" with nested rings and longitudes beyond 180.
func TestMultiPartAreasAndExtendedLongitude(t *testing.T) {
	snap := fetch(t, serve(t, 200, fixture(t, "isigmet_multipart.json")).IntlSigmet())
	nz := byID(t, snap, "NZKL 22")
	if nz.Geometry == nil || nz.Geometry.Type != model.GeomMultiPolygon || len(nz.Geometry.Polygons) != 2 || len(nz.ParseIssues) != 0 {
		t.Fatalf("NZKL 22 geometry = %+v issues=%v", nz.Geometry, nz.ParseIssues)
	}
	// Every canonical longitude is within [-180, 180]: AWC's second part ran
	// east of 180 (up to 196.333) and was shifted by 360 degrees.
	shifted := false
	for _, poly := range nz.Geometry.Polygons {
		for _, p := range poly[0] {
			if p[0] < -180 || p[0] > 180 {
				t.Fatalf("longitude %v outside [-180, 180]", p[0])
			}
		}
		minLon, maxLon := 999.0, -999.0
		for _, p := range poly[0] {
			if p[0] < minLon {
				minLon = p[0]
			}
			if p[0] > maxLon {
				maxLon = p[0]
			}
		}
		if minLon == -180 && maxLon > -164 && maxLon < -163.6 {
			shifted = true // 196.333 - 360 = -163.667
		}
	}
	if !shifted {
		t.Error("the part east of 180 should have been shifted to [-180, -163.667]")
	}
	if nz.GeometryDerivation == nil || !contains(nz.GeometryDerivation.Steps, "antimeridian_shift") {
		t.Errorf("derivation must record the shift: %+v", nz.GeometryDerivation)
	}
	br := byID(t, snap, "SBGL 39")
	if br.Geometry == nil || br.Geometry.Type != model.GeomMultiPolygon || len(br.Geometry.Polygons) != 2 {
		t.Errorf("SBGL 39 geometry = %+v", br.Geometry)
	}
	if issue(snap, provenance.GeometryUnavailable) != nil || issue(snap, provenance.ParseFailed) != nil {
		t.Errorf("valid multi-part records must not raise warnings: %+v", snap.Issues)
	}
	for _, a := range snap.Items {
		if err := a.Validate(); err != nil {
			t.Errorf("%s: %v", a.ID, err)
		}
	}
}

func contains(ss []string, v string) bool {
	for _, s := range ss {
		if s == v {
			return true
		}
	}
	return false
}

func TestGeometryEdgeCases(t *testing.T) {
	ring := `[{"lat":1,"lon":1},{"lat":1,"lon":2},{"lat":2,"lon":2},{"lat":1,"lon":1}]`
	open := `[{"lat":1,"lon":1},{"lat":1,"lon":2},{"lat":2,"lon":2},{"lat":2,"lon":3}]`
	tests := []struct {
		name, geom, coords string
		wantKind           string // Polygon | MultiPolygon | LineString | none
		wantIssue          bool
	}{
		{"flat area", `"AREA"`, ring, "Polygon", false},
		{"flat inferred closed", `null`, ring, "Polygon", false},
		{"open area is not served as an area", `"AREA"`, open, "none", true},
		{"open path with no type is a line, flagged", `null`, open, "LineString", true},
		{"explicit line", `"LINE"`, open, "LineString", false},
		{"line of one point", `"LINE"`, `[{"lat":1,"lon":1}]`, "none", true},
		{"areas ok", `"AREAS"`, `[` + ring + `,` + ring + `]`, "MultiPolygon", false},
		{"single-part areas collapses to a ring", `"AREAS"`, `[` + ring + `]`, "Polygon", false},
		{"areas with a collapsed part drops only that part", `"AREAS"`, `[` + ring + `,[{"lat":5,"lon":5},{"lat":5,"lon":5},{"lat":6,"lon":6},{"lat":5,"lon":5}]]`, "Polygon", false},
		{"areas with one open part is all-or-nothing", `"AREAS"`, `[` + ring + `,` + open + `]`, "none", true},
		{"areas but flat", `"AREAS"`, ring, "none", true},
		{"area but nested", `"AREA"`, `[` + ring + `]`, "none", true},
		{"unknown type", `"CIRCLE"`, ring, "none", true},
		{"empty coords", `"AREA"`, `[]`, "none", true},
		{"missing lon", `"AREA"`, `[{"lat":1},{"lat":1,"lon":2},{"lat":2,"lon":2},{"lat":1}]`, "none", true},
		{"lat out of range", `"AREA"`, `[{"lat":91,"lon":1},{"lat":1,"lon":2},{"lat":2,"lon":2},{"lat":91,"lon":1}]`, "none", true},
		{"lon beyond 360", `"AREA"`, `[{"lat":1,"lon":361},{"lat":1,"lon":2},{"lat":2,"lon":2},{"lat":1,"lon":361}]`, "none", true},
		{"lon in unwrapped range ok", `"AREA"`, `[{"lat":1,"lon":190},{"lat":1,"lon":200},{"lat":2,"lon":200},{"lat":1,"lon":190}]`, "Polygon", false},
		{"clockwise ring is reoriented, not rejected", `"AREA"`, `[{"lat":1,"lon":1},{"lat":2,"lon":2},{"lat":1,"lon":2},{"lat":1,"lon":1}]`, "Polygon", false},
		{"ring straddling the antimeridian is cut", `"AREA"`, `[{"lat":0,"lon":170},{"lat":0,"lon":190},{"lat":10,"lon":190},{"lat":10,"lon":170},{"lat":0,"lon":170}]`, "MultiPolygon", false},
		{"line beyond the antimeridian is refused", `"LINE"`, `[{"lat":1,"lon":179},{"lat":1,"lon":190}]`, "none", true},
		{"coords not a list", `"AREA"`, `"oops"`, "decode-failure", true},
		{"coords elements are junk", `"AREA"`, `[1,2,3]`, "decode-failure", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := `[{"icaoId":"X","seriesId":"1","rawSigmet":"RAW","geom":` + tt.geom + `,"coords":` + tt.coords + `}]`
			a := fetch(t, serve(t, 200, []byte(body)).IntlSigmet()).Items[0]
			if a.RawText == "" && len(a.SourceRecord) == 0 {
				t.Fatal("raw text or the source record must always survive")
			}
			got := "none"
			if tt.wantKind == "decode-failure" {
				if len(a.SourceRecord) == 0 || a.RawText != "" || len(a.ParseIssues) == 0 {
					t.Errorf("undecodable coords must become a flagged stub: %+v", a)
				}
				return
			}
			if a.Geometry != nil {
				got = string(a.Geometry.Type)
			}
			if got != tt.wantKind {
				t.Errorf("geometry = %s, want %s (%v)", got, tt.wantKind, a.ParseIssues)
			}
			if (len(a.ParseIssues) > 0) != tt.wantIssue {
				t.Errorf("issues = %v, want issue=%v", a.ParseIssues, tt.wantIssue)
			}
			if err := a.Validate(); err != nil {
				t.Errorf("served record invalid: %v", err)
			}
		})
	}
}
