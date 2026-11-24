package awc

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/davidhinkes/hobbs/internal/model"
	"github.com/davidhinkes/hobbs/internal/provenance"
	"github.com/davidhinkes/hobbs/internal/source"
)

// serveCapture is serve, but also records the query of the last request.
func serveCapture(t *testing.T, status int, body []byte, query *string) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*query = r.URL.Path + "?" + r.URL.RawQuery
		w.WriteHeader(status)
		w.Write(body)
	}))
	t.Cleanup(srv.Close)
	c := New()
	c.BaseURL = srv.URL
	c.Now = func() time.Time { return fixedNow }
	return c
}

func stationIssue(snap source.Snapshot[model.StationReport], code provenance.Code) *provenance.Issue {
	for i := range snap.Issues {
		if snap.Issues[i].Code == code {
			return &snap.Issues[i]
		}
	}
	return nil
}

func stationByID(t *testing.T, snap source.Snapshot[model.StationReport], id string) model.StationReport {
	t.Helper()
	for _, r := range snap.Items {
		if r.ID == id {
			return r
		}
	}
	t.Fatalf("no record %q", id)
	return model.StationReport{}
}

func TestMETARFixture(t *testing.T) {
	var q string
	c := serveCapture(t, 200, fixture(t, "metar.json"), &q)
	snap, err := c.METARs(context.Background(), []string{"KSMO", "KSNA", "KLAX"})
	if err != nil {
		t.Fatal(err)
	}
	if q != "/metar?ids=KSMO,KSNA,KLAX&format=json" {
		t.Errorf("request = %q", q)
	}
	if snap.Source.ID != "awc.metar" || snap.Source.RecordsInFeed != 5 || snap.Source.RecordsReturned != 5 || len(snap.Items) != 5 {
		t.Errorf("source = %+v, items = %d", snap.Source, len(snap.Items))
	}
	obs := time.Unix(1795499580, 0).UTC()
	r := stationByID(t, snap, "METAR KLAX "+obs.Format(time.RFC3339))
	if r.Kind != model.KindMETAR || r.ReportType != "METAR" || r.Station != "KLAX" || r.RawFormat != model.RawMETAR {
		t.Errorf("report = %+v", r)
	}
	if r.ObservedAt == nil || !r.ObservedAt.Equal(obs) {
		t.Errorf("observed_at = %v", r.ObservedAt)
	}
	if !strings.HasPrefix(r.RawText, "METAR KLAX 240553Z") {
		t.Errorf("raw text = %q", r.RawText)
	}
	if r.Geometry == nil || r.Geometry.Type != model.GeomPoint || r.Geometry.Point != (model.Position{-118.3866, 33.9382}) {
		t.Errorf("geometry = %+v", r.Geometry)
	}
	if r.ElevationM == nil || *r.ElevationM != 30 {
		t.Errorf("elevation = %v", r.ElevationM)
	}
	if r.ReportedAt == nil || r.ReceivedAt == nil {
		t.Errorf("reported/received = %v %v", r.ReportedAt, r.ReceivedAt)
	}
	if d := string(r.Details); !strings.Contains(d, `"fltCat"`) || strings.Contains(d, `"rawOb"`) || strings.Contains(d, `"icaoId"`) {
		t.Errorf("details = %s", d)
	}
	var speci *model.StationReport
	for i := range snap.Items {
		if snap.Items[i].ReportType == "SPECI" {
			speci = &snap.Items[i]
		}
	}
	if speci == nil || speci.Station != "KSMO" {
		t.Fatalf("no SPECI: %+v", speci)
	}
}

func TestMETARBadRecord(t *testing.T) {
	var q string
	c := serveCapture(t, 200, fixture(t, "metar.json"), &q)
	snap, err := c.METARs(context.Background(), []string{"KSMO", "KSNA", "KLAX"})
	if err != nil {
		t.Fatal(err)
	}
	var bad *model.StationReport
	for i := range snap.Items {
		if snap.Items[i].Station == "KBAD" {
			bad = &snap.Items[i]
		}
	}
	if bad == nil || len(bad.ParseIssues) == 0 || len(bad.SourceRecord) == 0 {
		t.Fatalf("bad record = %+v", bad)
	}
	pf := stationIssue(snap, provenance.ParseFailed)
	if pf == nil || len(pf.Refs) != 1 || pf.Refs[0] != bad.ID {
		t.Errorf("PARSE_FAILED = %+v (bad id %q)", pf, bad.ID)
	}
}

func TestTAFFixture(t *testing.T) {
	var q string
	c := serveCapture(t, 200, fixture(t, "taf.json"), &q)
	snap, err := c.TAFs(context.Background(), []string{"KSNA", "KSMO"})
	if err != nil {
		t.Fatal(err)
	}
	if q != "/taf?ids=KSNA,KSMO&format=json" {
		t.Errorf("request = %q", q)
	}
	if snap.Source.ID != "awc.taf" || len(snap.Items) != 2 {
		t.Fatalf("source = %+v, items = %d", snap.Source, len(snap.Items))
	}
	issued := time.Date(2026, 11, 24, 5, 39, 0, 0, time.UTC)
	r := stationByID(t, snap, "TAF KSMO "+issued.Format(time.RFC3339))
	if r.Kind != model.KindTAF || r.RawFormat != model.RawTAF || r.Station != "KSMO" {
		t.Errorf("report = %+v", r)
	}
	if r.IssuedAt == nil || !r.IssuedAt.Equal(issued) {
		t.Errorf("issued_at = %v", r.IssuedAt)
	}
	if r.ValidFrom == nil || !r.ValidFrom.Equal(time.Unix(1795500000, 0).UTC()) || r.ValidTo == nil || !r.ValidTo.Equal(time.Unix(1795586400, 0).UTC()) {
		t.Errorf("validity = %v .. %v", r.ValidFrom, r.ValidTo)
	}
	if !strings.HasPrefix(r.RawText, "TAF KSMO 240539Z") {
		t.Errorf("raw text = %q", r.RawText)
	}
	if d := string(r.Details); !strings.Contains(d, `"fcsts"`) || strings.Contains(d, `"rawTAF"`) {
		t.Errorf("details has wrong keys")
	}
}

func TestStationEmptyAnswersAreNotFailures(t *testing.T) {
	cases := []struct {
		name   string
		taf    bool
		status int
		body   string
	}{
		{"metar 204", false, 204, ""},
		{"taf 200 empty array", true, 200, "[]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := serve(t, tc.status, []byte(tc.body))
			var snap source.Snapshot[model.StationReport]
			var err error
			if tc.taf {
				snap, err = c.TAFs(context.Background(), []string{"ksmo", "KSNA"})
			} else {
				snap, err = c.METARs(context.Background(), []string{"ksmo", "KSNA"})
			}
			if err != nil || len(snap.Items) != 0 {
				t.Fatalf("err = %v, items = %d", err, len(snap.Items))
			}
			is := stationIssue(snap, provenance.StationNoData)
			if is == nil || is.Severity != provenance.Warning || strings.Join(is.Refs, ",") != "KSMO,KSNA" {
				t.Errorf("STATION_NO_DATA = %+v", is)
			}
		})
	}
}

func TestStationReportsMissingAndExtraStations(t *testing.T) {
	body := []byte(`[{"icaoId":"KSNA","obsTime":1795499580,"metarType":"METAR","rawOb":"METAR KSNA 240553Z"},
	{"icaoId":"KLAX","obsTime":1795499580,"metarType":"METAR","rawOb":"METAR KLAX 240553Z"}]`)
	snap, err := serve(t, 200, body).METARs(context.Background(), []string{"KSNA", "KXYZ"})
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Items) != 2 {
		t.Fatalf("items = %d", len(snap.Items))
	}
	is := stationIssue(snap, provenance.StationNoData)
	if is == nil || len(is.Refs) != 1 || is.Refs[0] != "KXYZ" {
		t.Fatalf("STATION_NO_DATA = %+v", is)
	}
	for _, want := range []string{"no current report", "no TAF published here", "unknown station"} {
		if !strings.Contains(is.Message, want) {
			t.Errorf("message %q lacks %q", is.Message, want)
		}
	}
}

func TestStationFailuresAreErrors(t *testing.T) {
	for name, tc := range map[string]struct {
		status int
		body   string
	}{
		"500":          {500, "boom"},
		"empty 200":    {200, ""},
		"not an array": {200, `{"not":"an array"}`},
	} {
		t.Run(name, func(t *testing.T) {
			snap, err := serve(t, tc.status, []byte(tc.body)).METARs(context.Background(), []string{"KSMO"})
			if !errors.Is(err, ErrUpstream) {
				t.Fatalf("err = %v", err)
			}
			if snap.Source.HTTPStatus != tc.status || snap.Source.ID != "awc.metar" {
				t.Errorf("source = %+v", snap.Source)
			}
		})
	}
}
