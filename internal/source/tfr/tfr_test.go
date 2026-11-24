package tfr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/davidhinkes/hobbs/internal/model"
	"github.com/davidhinkes/hobbs/internal/provenance"
)

var fixedNow = time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)

func td(t *testing.T, parts ...string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(append([]string{"..", "..", "..", "testdata", "tfr"}, parts...)...))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// fixtureIDs maps the five fixture TFRs to their detail files.
var fixtureIDs = map[string]string{
	"0/0367": "detail_textonly_0_0367.xml",
	"4/9383": "detail_circle_4_9383.xml",
	"5/3678": "detail_poly_5_3678.xml",
	"6/2481": "detail_multiarea_6_2481.xml",
	"6/2520": "detail_arc_6_2520.xml",
}

type detail struct {
	status int
	body   []byte
}

type site struct {
	srv        *httptest.Server
	listBody   []byte
	listStatus int
	details    map[string]detail // by notam id
	requests   sync.Map          // path -> count
	inflight   atomic.Int32
	maxFlight  atomic.Int32
	delay      time.Duration
}

// listFor builds a list from the real list fixture for the given ids.
func listFor(t *testing.T, ids ...string) []byte {
	t.Helper()
	var all []map[string]any
	if err := json.Unmarshal(td(t, "list.json"), &all); err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	for _, id := range ids {
		for _, e := range all {
			if e["notam_id"] == id {
				out = append(out, e)
			}
		}
	}
	b, _ := json.Marshal(out)
	return b
}

func newSite(t *testing.T, list []byte, details map[string]detail) (*site, *Client) {
	t.Helper()
	s := &site{listBody: list, listStatus: 200, details: details}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, _ := s.requests.LoadOrStore(r.URL.Path, new(atomic.Int32))
		n.(*atomic.Int32).Add(1)
		if r.URL.Path == "/tfrapi/getTfrList" {
			w.Header().Set("ETag", `"list"`)
			w.WriteHeader(s.listStatus)
			w.Write(s.listBody)
			return
		}
		cur := s.inflight.Add(1)
		for {
			m := s.maxFlight.Load()
			if cur <= m || s.maxFlight.CompareAndSwap(m, cur) {
				break
			}
		}
		defer s.inflight.Add(-1)
		time.Sleep(s.delay)
		for id, d := range s.details {
			if r.URL.Path == detailPath(id) {
				w.WriteHeader(d.status)
				w.Write(d.body)
				return
			}
		}
		w.WriteHeader(404)
	}))
	t.Cleanup(s.srv.Close)
	c := New()
	c.Enabled = true
	c.BaseURL = s.srv.URL
	c.Now = func() time.Time { return fixedNow }
	return s, c
}

func fixtureSite(t *testing.T) (*site, *Client) {
	t.Helper()
	d := map[string]detail{}
	var ids []string
	for id, f := range fixtureIDs {
		d[id] = detail{200, td(t, f)}
		ids = append(ids, id)
	}
	return newSite(t, listFor(t, ids...), d)
}

func byID(t *testing.T, items []model.TFR, id string) model.TFR {
	t.Helper()
	for _, x := range items {
		if x.ID == id {
			return x
		}
	}
	t.Fatalf("no TFR %q", id)
	return model.TFR{}
}

func issue(is []provenance.Issue, c provenance.Code) *provenance.Issue {
	for i := range is {
		if is[i].Code == c {
			return &is[i]
		}
	}
	return nil
}

func utc(y int, mo time.Month, d, h, mi int) *time.Time {
	t := time.Date(y, mo, d, h, mi, 0, 0, time.UTC)
	return &t
}

func sameTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}

func TestFixtureTFRs(t *testing.T) {
	_, c := fixtureSite(t)
	snap, err := c.Fetch(context.Background(), Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Items) != 5 || snap.Source.RecordsInFeed != 5 || snap.Source.RecordsReturned != 5 {
		t.Fatalf("items=%d feed=%d", len(snap.Items), snap.Source.RecordsInFeed)
	}
	for _, x := range snap.Items {
		if !x.DetailRetrieved || x.RawFormat != model.RawUSNotam || x.SourceRecord != nil || !strings.HasPrefix(x.RawText, "!FDC "+x.ID) {
			t.Errorf("%s: detail=%v raw=%.30q", x.ID, x.DetailRetrieved, x.RawText)
		}
		// USNS in RawText; only the other rendering in Translations.
		if len(x.Translations) != 1 || x.Translations[0].Format != "TRADITIONAL" || x.Translations[0].Text == x.RawText {
			t.Errorf("%s: translations=%v", x.ID, x.Translations)
		}
		if x.Regulation != "99.7" {
			t.Errorf("%s: regulation = %q", x.ID, x.Regulation)
		}
		if err := x.Validate(); err != nil {
			t.Errorf("%s: %v", x.ID, err)
		}
	}

	// List fields carry through as published.
	if x := byID(t, snap.Items, "6/2520"); x.Facility != "ZUA" || x.State != "GU" || x.Type != "SECURITY" {
		t.Errorf("list fields: %+v", x)
	}

	// Dates: converted to UTC from each TFR's own stated zone; raw kept.
	cases := []struct {
		id                string
		eff, exp          *time.Time
		effRaw, tz, expTZ string
	}{
		{"5/3678", utc(2026, 1, 1, 10, 0), utc(2027, 1, 1, 11, 59), "2026-01-01T05:00:00", "CDT", "CST"},
		{"6/2481", utc(2026, 11, 1, 19, 0), utc(2027, 3, 14, 22, 0), "2026-11-01T10:00:00", "AKST", "AKST"},
		{"6/2520", nil, utc(2027, 5, 22, 8, 59), "", "Guam", "Guam"},
		{"0/0367", utc(2020, 1, 15, 0, 1), nil, "2020-01-15T00:01:00", "UTC", "UTC"},
		{"4/9383", nil, nil, "", "UTC", "UTC"},
	}
	for _, tt := range cases {
		x := byID(t, snap.Items, tt.id)
		if !sameTime(x.Effective, tt.eff) || !sameTime(x.Expire, tt.exp) || x.EffectiveRaw != tt.effRaw || x.TimeZone != tt.tz || x.ExpireTimeZone != tt.expTZ {
			t.Errorf("%s: effective=%v expire=%v raw=%q tz=%q/%q", tt.id, x.Effective, x.Expire, x.EffectiveRaw, x.TimeZone, x.ExpireTimeZone)
		}
	}

	// Areas, altitudes and outlines against values computed independently from the XML.
	type want struct {
		id, name  string
		lower, up model.Altitude
		vertices  int // published outline vertices; 0 means no outline
		scheduled bool
	}
	areas := map[string][]want{
		"5/3678": {{"1038", "Area A", model.AGL(0), model.AGL(2000), 10, false}},
		"6/2520": {{"24008", "", model.AGL(0), model.MSL(6000), 34, false}},
		"4/9383": {{"3053", "Area", model.AGL(0), model.MSL(17999), 73, false}, {"3055", "Area B", model.AGL(0), model.MSL(17999), 72, false}},
		"6/2481": {
			{"1715", "A1500", model.AGL(1500), model.FL(430), 16, false},
			{"1717", "B1000", model.AGL(1000), model.FL(430), 88, false},
			{"1719", "C", model.AGL(2600), model.FL(430), 54, false},
			{"1721", "D FL320", model.FL(320), model.FL(430), 80, false},
			{"1723", "Area E", model.AGL(1000), model.AGL(2600), 54, true},
		},
		"0/0367": {{"177", "", model.AGL(0), model.AGL(0), 0, false}},
	}
	for id, ws := range areas {
		x := byID(t, snap.Items, id)
		if len(x.Areas) != len(ws) {
			t.Fatalf("%s: areas = %d, want %d", id, len(x.Areas), len(ws))
		}
		for i, w := range ws {
			a := x.Areas[i]
			if a.ID != w.id || a.Name != w.name || !altEq(a.Lower, w.lower) || !altEq(a.Upper, w.up) || a.Scheduled != w.scheduled {
				t.Errorf("%s area %d: %+v", id, i, a)
			}
			if w.vertices == 0 {
				if a.Geometry != nil {
					t.Errorf("%s area %d: text-only area must have no geometry", id, i)
				}
				continue
			}
			if a.Geometry == nil || a.Geometry.Type != model.GeomPolygon {
				t.Fatalf("%s area %d: geometry = %+v", id, i, a.Geometry)
			}
			if got := len(a.Geometry.Polygons[0][0]); got != w.vertices {
				t.Errorf("%s area %d: outline has %d vertices, the FAA published %d", id, i, got, w.vertices)
			}
			if a.GeometryDerivation != nil && a.GeometryDerivation.Circle != nil {
				t.Errorf("%s area %d: outlines are used as published, no circle math", id, i)
			}
			// The raw altitude fields are kept so the reading can be redone.
			if a.Details["upper_value"] == "" || a.Details["lower_value"] == "" {
				t.Errorf("%s area %d: raw altitude fields missing: %v", id, i, a.Details)
			}
		}
	}
	// First published vertex of 4/9383 area A: 39.35990574N 077.03638889W, as [lon, lat].
	if p := byID(t, snap.Items, "4/9383").Areas[0].Geometry.Polygons[0][0][0]; p != (model.Position{-77.03638889, 39.35990574}) {
		t.Errorf("first vertex = %v", p)
	}
	// East longitudes stay east (Guam): 144.84472222E.
	if p := byID(t, snap.Items, "6/2520").Areas[0].Geometry.Polygons[0][0][0]; p != (model.Position{144.84472222, 13.56361111}) {
		t.Errorf("Guam first vertex = %v", p)
	}
	// A scheduled area passes its schedule through, uninterpreted.
	e := byID(t, snap.Items, "6/2481").Areas[4]
	if got := strings.Join(e.DayCodes, ","); got != "Tuesday,Thursday,Saturday" {
		t.Errorf("day codes = %q: the FAA repeats the element and every value must survive", got)
	}
	if len(e.Schedules) != 1 || e.Schedules[0].StartTime != "2026-11-01T11:00:00" || e.Schedules[0].TimeSeparate != "TRUE" {
		t.Errorf("schedule passed through as published: %+v", e.Schedules)
	}
	if is := issue(snap.Issues, provenance.ScheduleUnevaluable); is == nil || len(is.Refs) != 1 || is.Refs[0] != "6/2481" {
		t.Errorf("SCHEDULE_UNEVALUABLE = %+v", is)
	}
	// The text-only TFR is flagged, never silently dropped.
	if is := issue(snap.Issues, provenance.GeometryUnavailable); is == nil || len(is.Refs) != 1 || is.Refs[0] != "0/0367" || is.Severity != provenance.Warning {
		t.Errorf("GEOMETRY_UNAVAILABLE = %+v", is)
	}
	if issue(snap.Issues, provenance.ParseFailed) != nil || issue(snap.Issues, provenance.UpstreamError) != nil {
		t.Errorf("clean fixtures should not raise parse/upstream warnings: %+v", snap.Issues)
	}
	// Attribution: undocumented source, whole list hashed.
	s := snap.Source
	if s.ID != SourceID || s.Documentation != provenance.Undocumented || s.HTTPStatus != 200 || s.ContentSHA256 == "" || s.ETag != `"list"` || !s.RetrievedAt.Equal(fixedNow) {
		t.Errorf("source = %+v", s)
	}
	env, err := provenance.New(snap.Items, fixedNow, []provenance.Source{s}, nil, snap.Issues)
	if err != nil {
		t.Fatal(err)
	}
	if issue(env.Completeness.Issues, provenance.UndocumentedSource) == nil || env.Completeness.Status != provenance.Degraded {
		t.Errorf("envelope = %+v", env.Completeness)
	}
}

func TestListFailuresAreErrors(t *testing.T) {
	for name, tt := range map[string]struct {
		status int
		body   string
		want   string
	}{
		"server error":     {500, "oops", "HTTP 500"},
		"not found":        {404, "", "HTTP 404"},
		"html page":        {200, "<html>down</html>", "not a JSON array"},
		"object not array": {200, `{"error":"x"}`, "not a JSON array"},
		"empty body":       {200, "", "not a JSON array"},
	} {
		t.Run(name, func(t *testing.T) {
			s, c := newSite(t, []byte(tt.body), nil)
			s.listStatus = tt.status
			snap, err := c.Fetch(context.Background(), Filter{})
			if !errors.Is(err, ErrUpstream) || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v", err)
			}
			if snap.Source.URL == "" || snap.Source.RetrievedAt.IsZero() || len(snap.Items) != 0 {
				t.Errorf("failure must be attributable: %+v", snap.Source)
			}
		})
	}
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	c := New()
	c.Enabled = true
	c.BaseURL = url
	c.Now = func() time.Time { return fixedNow }
	if snap, err := c.Fetch(context.Background(), Filter{}); !errors.Is(err, ErrUpstream) || snap.Source.RetrievedAt.IsZero() {
		t.Errorf("unreachable: %v", err)
	}
}

func TestEmptyListIsAGenuineNone(t *testing.T) {
	_, c := newSite(t, []byte(`[]`), nil)
	snap, err := c.Fetch(context.Background(), Filter{})
	if err != nil || len(snap.Items) != 0 || snap.Source.RecordsInFeed != 0 {
		t.Fatalf("snap=%+v err=%v", snap.Source, err)
	}
}

// A TFR whose detail cannot be had is still returned, with what the list says,
// and the response says so. Nothing is dropped.
func TestDetailFailuresKeepTheTFR(t *testing.T) {
	list := listFor(t, "4/9383", "5/3678", "6/2481")
	d := map[string]detail{
		"4/9383": {500, []byte("boom")},
		"5/3678": {200, []byte("<not xml")},
		"6/2481": {200, td(t, "detail_multiarea_6_2481.xml")},
	}
	_, c := newSite(t, list, d)
	snap, err := c.Fetch(context.Background(), Filter{})
	if err != nil || len(snap.Items) != 3 {
		t.Fatalf("items=%d err=%v", len(snap.Items), err)
	}
	for _, id := range []string{"4/9383", "5/3678"} {
		x := byID(t, snap.Items, id)
		if x.DetailRetrieved || x.RawText != "" || x.RawFormat != "" || !strings.Contains(string(x.SourceRecord), `"notam_id"`) || len(x.ParseIssues) == 0 {
			t.Errorf("%s: %+v", id, x)
		}
		if x.Type == "" || x.Facility == "" || x.Description == "" {
			t.Errorf("%s: list fields must survive: %+v", id, x)
		}
		if err := x.Validate(); err != nil {
			t.Errorf("%s: %v", id, err)
		}
	}
	if x := byID(t, snap.Items, "6/2481"); !x.DetailRetrieved {
		t.Error("a healthy neighbour must not be damaged")
	}
	if is := issue(snap.Issues, provenance.UpstreamError); is == nil || len(is.Refs) != 1 || is.Refs[0] != "4/9383" {
		t.Errorf("UPSTREAM_ERROR = %+v", is)
	}
	if is := issue(snap.Issues, provenance.ParseFailed); is == nil || len(is.Refs) != 1 || is.Refs[0] != "5/3678" {
		t.Errorf("PARSE_FAILED = %+v", is)
	}
	env, _ := provenance.New(snap.Items, fixedNow, []provenance.Source{snap.Source}, nil, snap.Issues)
	if env.Completeness.CanAssertNone() {
		t.Error("a response with missing detail must not assert completeness")
	}
}

func TestFilterIsExactMatchAndOnlyFetchesWhatItNeeds(t *testing.T) {
	_, c := fixtureSite(t)
	cases := map[string]struct {
		f    Filter
		want []string
	}{
		"state":           {Filter{State: "ak"}, []string{"6/2481"}},
		"facility":        {Filter{Facility: "ZDC"}, []string{"4/9383"}},
		"type":            {Filter{Type: "security"}, nil}, // all five are SECURITY except noted below
		"ids":             {Filter{IDs: []string{"5/3678", "0/0367"}}, []string{"0/0367", "5/3678"}},
		"combined":        {Filter{State: "GU", Type: "SECURITY"}, []string{"6/2520"}},
		"no match":        {Filter{State: "ZZ"}, []string{}},
		"not a substring": {Filter{State: "A"}, []string{}},
	}
	// Find which are actually SECURITY to build the expectation for "type".
	all, _ := c.Fetch(context.Background(), Filter{})
	for _, x := range all.Items {
		if strings.EqualFold(x.Type, "SECURITY") {
			tc := cases["type"]
			tc.want = append(tc.want, x.ID)
			cases["type"] = tc
		}
	}
	for name, tt := range cases {
		t.Run(name, func(t *testing.T) {
			snap, err := c.Fetch(context.Background(), tt.f)
			if err != nil {
				t.Fatal(err)
			}
			got := map[string]bool{}
			for _, x := range snap.Items {
				got[x.ID] = true
			}
			if len(got) != len(tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
			for _, id := range tt.want {
				if !got[id] {
					t.Errorf("missing %s in %v", id, got)
				}
			}
			if snap.Source.RecordsInFeed != 5 || snap.Source.RecordsReturned != len(tt.want) {
				t.Errorf("feed=%d returned=%d", snap.Source.RecordsInFeed, snap.Source.RecordsReturned)
			}
		})
	}
	// Only matching detail documents are requested.
	s, c2 := fixtureSite(t)
	if _, err := c2.Fetch(context.Background(), Filter{IDs: []string{"5/3678"}}); err != nil {
		t.Fatal(err)
	}
	n := 0
	s.requests.Range(func(k, v any) bool {
		if strings.HasPrefix(k.(string), "/download/") {
			n += int(v.(*atomic.Int32).Load())
		}
		return true
	})
	if n != 1 {
		t.Errorf("detail requests = %d, want 1", n)
	}
}

func TestDetailRequestsAreBounded(t *testing.T) {
	var entries []map[string]string
	d := map[string]detail{}
	body := td(t, "detail_poly_5_3678.xml")
	for i := 0; i < 40; i++ {
		id := fmt.Sprintf("7/%04d", i)
		entries = append(entries, map[string]string{"notam_id": id, "facility": "ZXX", "state": "TX", "type": "HAZARDS", "description": "x"})
		d[id] = detail{200, body}
	}
	list, _ := json.Marshal(entries)
	s, c := newSite(t, list, d)
	s.delay = 15 * time.Millisecond
	snap, err := c.Fetch(context.Background(), Filter{})
	if err != nil || len(snap.Items) != 40 {
		t.Fatalf("items=%d err=%v", len(snap.Items), err)
	}
	if m := s.maxFlight.Load(); m > concurrency || m < 2 {
		t.Errorf("max in flight = %d, want 2..%d", m, concurrency)
	}
	// Order follows the list.
	for i, x := range snap.Items {
		if x.ID != fmt.Sprintf("7/%04d", i) {
			t.Fatalf("item %d is %s: order must follow the list", i, x.ID)
		}
	}
}

// ---- parser unit tests on hand-built XML ----

func doc(notBody string) []byte {
	return []byte(`<?xml version="1.0"?><XNOTAM-Update><Group><Add><Not>` + notBody + `</Not></Add></Group></XNOTAM-Update>`)
}

func area(id, extra string) string {
	return `<aseTFRArea><AseUid><codeType>RAS</codeType><codeId>` + id + `</codeId></AseUid><txtName>A` + id + `</txtName>` +
		`<codeDistVerUpper>ALT</codeDistVerUpper><valDistVerUpper>5000</valDistVerUpper><uomDistVerUpper>FT</uomDistVerUpper>` +
		`<codeDistVerLower>HEI</codeDistVerLower><valDistVerLower>0</valDistVerLower><uomDistVerLower>FT</uomDistVerLower>` + extra + `</aseTFRArea>`
}

func avx(typ, lat, lon string) string {
	return `<Avx><codeDatum>WGE</codeDatum><codeType>` + typ + `</codeType><geoLat>` + lat + `</geoLat><geoLong>` + lon + `</geoLong></Avx>`
}

func merged(id string, vs ...string) string {
	return `<abdMergedArea><AbdUid><AseUid><codeId>` + id + `</codeId></AseUid></AbdUid>` + strings.Join(vs, "") + `</abdMergedArea>`
}

func square(typ string, lon0, lon1 string) []string {
	return []string{avx(typ, "10.0N", lon0), avx(typ, "10.0N", lon1), avx(typ, "11.0N", lon1), avx(typ, "11.0N", lon0), avx(typ, "10.0N", lon0)}
}

func parse(t *testing.T, x []byte) (model.TFR, *note, error) {
	t.Helper()
	tf := model.TFR{ID: "9/0001"}
	n := &note{}
	err := parseDetail(x, &tf, n)
	return tf, n, err
}

func notBody(areas, tz, eff string) string {
	return `<NotUid><txtLocalName>x</txtLocalName></NotUid><dateEffective>` + eff + `</dateEffective><codeTimeZone>` + tz + `</codeTimeZone>` +
		`<txtDescrUSNS>!FDC 9/0001 TEXT</txtDescrUSNS><TfrNot><codeType>91.137</codeType><TFRAreaGroup>` + areas + `</TFRAreaGroup></TfrNot>`
}

func TestParserRefusesWhatItCannotTranslateFaithfully(t *testing.T) {
	cases := map[string]struct {
		areas   string
		wantMsg string
	}{
		"non-geodetic vertex type":  {area("1", "") + merged("1", square("CIR", "100.0W", "99.0W")...), "not a plain geodetic point"},
		"wrapped antimeridian ring": {area("1", "") + merged("1", square("GRC", "179.5E", "179.5W")...), "antimeridian"},
		"bad hemisphere":            {area("1", "") + merged("1", avx("GRC", "10.0X", "100.0W"), avx("GRC", "10.0N", "99.0W"), avx("GRC", "11.0N", "99.0W"), avx("GRC", "10.0X", "100.0W")), "not a valid position"},
		"latitude beyond 90":        {area("1", "") + merged("1", avx("GRC", "95.0N", "100.0W"), avx("GRC", "10.0N", "99.0W"), avx("GRC", "11.0N", "99.0W"), avx("GRC", "95.0N", "100.0W")), "not a valid position"},
		"open ring":                 {area("1", "") + merged("1", avx("GRC", "10.0N", "100.0W"), avx("GRC", "10.0N", "99.0W"), avx("GRC", "11.0N", "99.0W"), avx("GRC", "11.0N", "100.0W")), "not a closed ring"},
		"too few points":            {area("1", "") + merged("1", avx("GRC", "10.0N", "100.0W"), avx("GRC", "10.0N", "99.0W"), avx("GRC", "10.0N", "100.0W")), "not a closed ring"},
		"non-WGS84 datum":           {area("1", "") + strings.Replace(merged("1", square("GRC", "100.0W", "99.0W")...), "WGE", "NAD", 1), "not WGS84"},
		"outline for unknown area":  {area("1", "") + merged("2", square("GRC", "100.0W", "99.0W")...), "no matching area"},
	}
	for name, tt := range cases {
		t.Run(name, func(t *testing.T) {
			tf, n, err := parse(t, doc(notBody(tt.areas, "UTC", "2026-11-01T00:00:00")))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(strings.Join(n.Issues, " | "), tt.wantMsg) {
				t.Errorf("issues = %v, want %q", n.Issues, tt.wantMsg)
			}
			for _, a := range tf.Areas {
				if a.Geometry != nil {
					t.Errorf("geometry must not be produced: %+v", a.Geometry)
				}
			}
			if tf.RawText == "" || !tf.DetailRetrieved {
				t.Errorf("the TFR itself must survive: %+v", tf)
			}
		})
	}
	// A clean square is accepted, wound correctly, with positions as [lon, lat].
	tf, n, _ := parse(t, doc(notBody(area("1", "")+merged("1", square("GRC", "100.0W", "99.0W")...), "UTC", "2026-11-01T00:00:00")))
	if len(n.Issues) != 0 || tf.Areas[0].Geometry == nil || tf.Areas[0].Geometry.Validate() != nil {
		t.Fatalf("clean square: %v %+v", n.Issues, tf.Areas[0].Geometry)
	}
	if p := tf.Areas[0].Geometry.Polygons[0][0][0]; p != (model.Position{-100, 10}) {
		t.Errorf("first vertex = %v", p)
	}
	// East, north, west is already counter-clockwise: nothing to translate.
	if d := tf.Areas[0].GeometryDerivation; d != nil {
		t.Errorf("a correctly wound outline needs no translation: %+v", d)
	}
	// The same square published clockwise is re-wound, and the derivation says so.
	cw := []string{avx("GRC", "10.0N", "100.0W"), avx("GRC", "11.0N", "100.0W"), avx("GRC", "11.0N", "99.0W"), avx("GRC", "10.0N", "99.0W"), avx("GRC", "10.0N", "100.0W")}
	tf, n, _ = parse(t, doc(notBody(area("1", "")+merged("1", cw...), "UTC", "2026-11-01T00:00:00")))
	if d := tf.Areas[0].GeometryDerivation; len(n.Issues) != 0 || d == nil || len(d.Steps) != 1 || d.Steps[0] != "ring_reoriented" {
		t.Errorf("derivation = %+v issues=%v", d, n.Issues)
	}
	if err := tf.Areas[0].Geometry.Validate(); err != nil {
		t.Error(err)
	}
}

func TestParserHandlesByteOrderMarkAndGarbage(t *testing.T) {
	good := doc(notBody(area("1", ""), "UTC", "2026-11-01T00:00:00"))
	if _, _, err := parse(t, append([]byte("\xef\xbb\xbf"), good...)); err != nil {
		t.Errorf("BOM must be tolerated: %v", err)
	}
	for name, b := range map[string][]byte{
		"empty":               nil,
		"not xml":             []byte("hello"),
		"truncated":           good[:len(good)/2],
		"valid XML, no NOTAM": []byte(`<?xml version="1.0"?><XNOTAM-Update><Group><Add></Add></Group></XNOTAM-Update>`),
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := parse(t, b); err == nil {
				t.Error("expected an error")
			}
		})
	}
}

func TestAltitudeMapping(t *testing.T) {
	tests := []struct {
		code, val, uom string
		want           model.Altitude
		flagged        bool
	}{
		{"ALT", "6000", "FT", model.MSL(6000), false},
		{"HEI", "1500", "FT", model.AGL(1500), false},
		{"HEI", "0", "FT", model.AGL(0), false},
		{"ALT", "430", "FL", model.FL(430), false},
		{"", "440", "FL", model.FL(440), false}, // live: space operations, code absent, unit FL
		{"HEI", "999", "FL", model.Unlimited(), false},
		{"ALT", "999", "FL", model.Unlimited(), false},
		{"", "0", "FT", model.Unknown(), true}, // live: no code with feet: datum unknown
		{"HEI", "100", "FL", model.Unknown(), true},
		{"ALT", "1500.5", "FT", model.Unknown(), true},
		{"ALT", "abc", "FT", model.Unknown(), true},
		{"ALT", "-5", "FT", model.Unknown(), true},
		{"MSL", "100", "FT", model.Unknown(), true},
		{"ALT", "100", "M", model.Unknown(), true},
		{"ALT", "", "FT", model.Unknown(), false}, // not stated: unknown, not an error
	}
	for _, tt := range tests {
		t.Run(tt.code+tt.val+tt.uom, func(t *testing.T) {
			n := &note{}
			got := altitude(tt.code, tt.val, tt.uom, "upper", n)
			if got.Datum != tt.want.Datum || (got.Value == nil) != (tt.want.Value == nil) || (got.Value != nil && *got.Value != *tt.want.Value) {
				t.Errorf("= %+v, want %+v", got, tt.want)
			}
			if (len(n.Issues) > 0) != tt.flagged {
				t.Errorf("issues = %v, flagged want %v", n.Issues, tt.flagged)
			}
			if err := got.Validate(); err != nil {
				t.Error(err)
			}
		})
	}
}

func TestTimeZoneConversion(t *testing.T) {
	tests := []struct {
		raw, zone string
		want      string // RFC 3339 UTC, or "" for not converted
		flagged   bool
	}{
		{"2026-11-22T12:00:00", "UTC", "2026-11-22T12:00:00Z", false},
		{"2026-01-01T05:00:00", "CDT", "2026-01-01T10:00:00Z", false},
		{"2027-01-01T05:59:00", "CST", "2027-01-01T11:59:00Z", false},
		{"2026-06-01T12:00:00", "EDT", "2026-06-01T16:00:00Z", false},
		{"2026-06-01T12:00:00", "pdt", "2026-06-01T19:00:00Z", false},
		{"2026-11-01T10:00:00", "AKST", "2026-11-01T19:00:00Z", false},
		{"2027-05-22T18:59:00", "Guam", "2027-05-22T08:59:00Z", false},
		{"2026-11-01T00:30:00", "Guam", "2026-10-31T14:30:00Z", false}, // crosses midnight backwards
		{"2026-11-01T23:30:00", "PST", "2026-11-02T07:30:00Z", false},  // crosses midnight forwards
		{"", "UTC", "", false},
		{"2026-11-22T12:00:00", "XYZ", "", true},
		{"2026-11-22T12:00:00", "", "", true},
		{"yesterday", "UTC", "", true},
		{"2026-11-22 12:00", "UTC", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.raw+"/"+tt.zone, func(t *testing.T) {
			n := &note{}
			got := toUTC(tt.raw, tt.zone, "effective", n)
			switch {
			case tt.want == "" && got != nil:
				t.Errorf("= %v, want not converted", got)
			case tt.want != "" && (got == nil || got.Format(time.RFC3339) != tt.want):
				t.Errorf("= %v, want %s", got, tt.want)
			}
			if (len(n.Issues) > 0) != tt.flagged {
				t.Errorf("issues = %v, flagged want %v", n.Issues, tt.flagged)
			}
		})
	}
	// An unconvertible time keeps its raw value on the record.
	tf, n, _ := parse(t, doc(notBody(area("1", ""), "MARS", "2026-11-01T00:00:00")))
	if tf.Effective != nil || tf.EffectiveRaw != "2026-11-01T00:00:00" || tf.TimeZone != "MARS" || len(n.Issues) == 0 {
		t.Errorf("tfr = %+v issues=%v", tf, n.Issues)
	}
}

func TestHemisphereParsing(t *testing.T) {
	ok := map[string]float64{"39.35990574N": 39.35990574, "077.03638889W": -77.03638889, "13.5S": -13.5, "144.8E": 144.8, "0N": 0, "90N": 90, "180W": -180}
	for s, want := range ok {
		lat := strings.ContainsAny(s, "NS")
		if got, good := parseHemi(s, lat); !good || got != want {
			t.Errorf("parseHemi(%q) = %v %v, want %v", s, got, good, want)
		}
	}
	for s, lat := range map[string]bool{"": true, "N": true, "39.3": true, "91N": true, "10W": true, "181E": false, "10N": false, "abcN": true, "-5N": true, "5 N": true} {
		if _, good := parseHemi(s, lat); good && s != "5 N" {
			t.Errorf("parseHemi(%q, lat=%v) should fail", s, lat)
		}
	}
}

func TestTFRsWithoutAreasAreFlagged(t *testing.T) {
	tf, n, err := parse(t, doc(`<NotUid><txtLocalName>x</txtLocalName></NotUid><txtDescrUSNS>!FDC 9/0001 TEXT</txtDescrUSNS>`))
	if err != nil || !tf.DetailRetrieved || tf.RawText == "" || len(n.Issues) == 0 || len(tf.Areas) != 0 {
		t.Errorf("tf=%+v issues=%v err=%v", tf, n.Issues, err)
	}
}

func altEq(a, b model.Altitude) bool {
	if a.Datum != b.Datum || (a.Value == nil) != (b.Value == nil) {
		return false
	}
	return a.Value == nil || *a.Value == *b.Value
}

// encoding/xml keeps only the last of a repeated single-valued element. The
// guard must flag every such repetition instead of letting data vanish.
func TestRepeatedSingleValuedElementsAreFlagged(t *testing.T) {
	areaWith := func(extra string) string { return area("1", extra) }
	cases := map[string]struct {
		not  string
		want string
	}{
		"repeated txtName":        {notBody(strings.Replace(areaWith(""), "<txtName>A1</txtName>", "<txtName>A1</txtName><txtName>B1</txtName>", 1), "UTC", "2026-11-01T00:00:00"), "txtName repeats 2 times under aseTFRArea"},
		"repeated altitude":       {notBody(strings.Replace(areaWith(""), "<valDistVerUpper>5000</valDistVerUpper>", "<valDistVerUpper>5000</valDistVerUpper><valDistVerUpper>9000</valDistVerUpper>", 1), "UTC", "2026-11-01T00:00:00"), "valDistVerUpper repeats 2 times"},
		"repeated effective date": {notBody(areaWith(""), "UTC", "2026-11-01T00:00:00") + "<dateEffective>2027-01-01T00:00:00</dateEffective>", "dateEffective repeats 2 times under Not"},
		"repeated text":           {notBody(areaWith(""), "UTC", "2026-11-01T00:00:00") + "<txtDescrUSNS>second</txtDescrUSNS>", "txtDescrUSNS repeats 2 times under Not"},
	}
	for name, tt := range cases {
		t.Run(name, func(t *testing.T) {
			_, n, err := parse(t, doc(tt.not))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(strings.Join(n.Issues, " | "), tt.want) {
				t.Errorf("issues = %v, want %q", n.Issues, tt.want)
			}
		})
	}
	// Elements that are expected to repeat do not trigger it.
	multi := area("1", "<dayCode>Monday</dayCode><dayCode>Friday</dayCode>") + area("2", "")
	tf, n, _ := parse(t, doc(notBody(multi, "UTC", "2026-11-01T00:00:00")+"<txtAddrPOCPhone>111</txtAddrPOCPhone><txtAddrPOCPhone>222</txtAddrPOCPhone>"))
	if len(n.Issues) != 0 {
		t.Errorf("expected-multiple elements must not be flagged: %v", n.Issues)
	}
	if got := strings.Join(tf.Areas[0].DayCodes, ","); got != "Monday,Friday" {
		t.Errorf("day codes = %q", got)
	}
	if tf.Details["poc_phone"] != "111; 222" {
		t.Errorf("every phone number must survive: %q", tf.Details["poc_phone"])
	}
	// The five real fixtures have no unexpected repetition.
	_, c := fixtureSite(t)
	snap, _ := c.Fetch(context.Background(), Filter{})
	for _, x := range snap.Items {
		for _, is := range x.ParseIssues {
			if strings.Contains(is, "repeats") {
				t.Errorf("%s: %s", x.ID, is)
			}
		}
	}
}

// Off by default: a client nobody enabled never touches the network.
func TestDisabledByDefault(t *testing.T) {
	s, _ := fixtureSite(t)
	c := New() // not enabled
	c.BaseURL = s.srv.URL
	snap, err := c.Fetch(context.Background(), Filter{})
	if !errors.Is(err, ErrDisabled) || len(snap.Items) != 0 {
		t.Fatalf("err = %v, items = %d", err, len(snap.Items))
	}
	n := 0
	s.requests.Range(func(_, _ any) bool { n++; return true })
	if n != 0 {
		t.Errorf("a disabled client made %d request(s)", n)
	}
	if _, err := c.Source().Fetch(context.Background()); !errors.Is(err, ErrDisabled) {
		t.Errorf("Source(): %v", err)
	}
}
