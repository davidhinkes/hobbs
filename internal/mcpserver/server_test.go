package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/davidhinkes/hobbs/internal/model"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/davidhinkes/hobbs/internal/source/awc"
	"github.com/davidhinkes/hobbs/internal/source/nms"
)

var fixedNow = time.Date(2026, 11, 23, 14, 0, 0, 0, time.UTC)

func fixture(t *testing.T, parts ...string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(append([]string{"..", "..", "testdata"}, parts...)...))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// upstreams are fake AWC and NMS servers.
type upstreams struct {
	awcSrv, nmsSrv *httptest.Server
	mu             sync.Mutex
	awcFail        map[string]int    // path -> status to return instead of the fixture
	nmsFail        int               // status to return for notam queries
	nmsFailFor     map[string]int    // location -> status, for that location only
	nmsBodyFor     map[string][]byte // location -> body instead of the fixture
	lastNotamQuery map[string]string
	notamQueries   []map[string]string // every query, in arrival order
	lastNotamHdr   http.Header
	awcCalls       map[string]int
	awcQueries     map[string]string // path -> raw query string of the last request
}

func newUpstreams(t *testing.T) *upstreams {
	t.Helper()
	u := &upstreams{awcFail: map[string]int{}, nmsFailFor: map[string]int{}, nmsBodyFor: map[string][]byte{}, lastNotamQuery: map[string]string{}, awcCalls: map[string]int{}, awcQueries: map[string]string{}}
	files := map[string]string{"/airsigmet": "airsigmet.json", "/gairmet": "gairmet.json", "/isigmet": "isigmet.json", "/cwa": "cwa.json", "/metar": "metar.json", "/taf": "taf.json"}
	u.awcSrv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		u.awcCalls[r.URL.Path]++
		u.awcQueries[r.URL.Path] = r.URL.RawQuery
		fail := u.awcFail[r.URL.Path]
		u.mu.Unlock()
		if fail != 0 {
			w.WriteHeader(fail)
			w.Write([]byte("down"))
			return
		}
		f, ok := files[r.URL.Path]
		if !ok {
			w.WriteHeader(404)
			return
		}
		w.Write(fixture(t, "awc", f))
	}))
	u.nmsSrv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/auth/token":
			w.Write([]byte(`{"access_token":"tok","expires_in":"1799"}`))
		case "/nmsapi/v1/ping":
			if u.nmsFail != 0 {
				w.WriteHeader(u.nmsFail)
				return
			}
			w.Write([]byte("Healthy 200 OK"))
		case "/nmsapi/v1/notams":
			u.mu.Lock()
			u.lastNotamHdr = r.Header.Clone()
			u.lastNotamQuery = map[string]string{}
			for k, v := range r.URL.Query() {
				u.lastNotamQuery[k] = v[0]
			}
			u.notamQueries = append(u.notamQueries, u.lastNotamQuery)
			fail := u.nmsFail
			loc := r.URL.Query().Get("location")
			if f := u.nmsFailFor[loc]; f != 0 {
				fail = f
			}
			body := u.nmsBodyFor[loc]
			u.mu.Unlock()
			if fail != 0 {
				w.WriteHeader(fail)
				w.Write([]byte(`{"status":"Failure","message":"boom"}`))
				return
			}
			if body == nil {
				body = fixture(t, "nms", "notams_sample.json")
			}
			w.Write(body)
		}
	}))
	t.Cleanup(func() { u.awcSrv.Close(); u.nmsSrv.Close() })
	return u
}

func (u *upstreams) deps(t *testing.T, withNMS, airworthy bool) Deps {
	t.Helper()
	a := awc.New()
	a.BaseURL = u.awcSrv.URL
	a.Now = func() time.Time { return fixedNow }
	d := Deps{AWC: a, Airworthy: airworthy, Now: func() time.Time { return fixedNow }}
	if withNMS {
		c, err := nms.NewClient(nms.Config{Host: u.nmsSrv.URL, ClientID: "id-0000000000", ClientSecret: "secret-0000000", Now: func() time.Time { return fixedNow }})
		if err != nil {
			t.Fatal(err)
		}
		d.NMS = c
	}
	return d
}

func connect(t *testing.T, d Deps) *mcp.ClientSession {
	t.Helper()
	srv, err := New(d)
	if err != nil {
		t.Fatal(err)
	}
	ct, st := mcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := srv.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

// reply is the part of the envelope the tests inspect.
type reply struct {
	Data        json.RawMessage `json:"data"`
	Airworthy   *bool           `json:"airworthy"`
	EvaluatedAt string          `json:"evaluated_at"`
	Disclaimer  string          `json:"disclaimer"`
	Provenance  struct {
		Sources []struct {
			ID              string `json:"id"`
			HTTPStatus      int    `json:"http_status"`
			RecordsInFeed   int    `json:"records_in_feed"`
			RecordsReturned int    `json:"records_returned"`
			ServedFrom      string `json:"served_from"`
		} `json:"sources"`
		NotConsulted []struct{ ID, Reason string } `json:"not_consulted"`
	} `json:"provenance"`
	Completeness struct {
		Status string `json:"status"`
		Issues []struct {
			Code     string   `json:"code"`
			Severity string   `json:"severity"`
			Source   string   `json:"source"`
			Message  string   `json:"message"`
			Refs     []string `json:"refs"`
		} `json:"issues"`
	} `json:"completeness"`
}

func (r reply) hasIssue(code, source string) bool {
	for _, i := range r.Completeness.Issues {
		if i.Code == code && (source == "" || i.Source == source) {
			return true
		}
	}
	return false
}

func call(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) (*mcp.CallToolResult, reply) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: protocol error: %v", name, err)
	}
	var r reply
	if !res.IsError || res.StructuredContent != nil {
		b, _ := json.Marshal(res.StructuredContent)
		if err := json.Unmarshal(b, &r); err != nil {
			t.Fatalf("%s: structured content is not an envelope: %v\n%s", name, err, b)
		}
	}
	return res, r
}

func text(res *mcp.CallToolResult) string {
	var sb strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			sb.WriteString(tc.Text)
		}
	}
	return sb.String()
}

func TestToolsAreListedWithTheContractInTheirDescriptions(t *testing.T) {
	cs := connect(t, newUpstreams(t).deps(t, true, true))
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]*mcp.Tool{}
	for _, tl := range res.Tools {
		got[tl.Name] = tl
	}
	for _, name := range []string{"get_weather_advisories", "get_notams", "source_status", "get_flight_briefing_data"} {
		tl := got[name]
		if tl == nil {
			t.Fatalf("missing tool %s (have %v)", name, got)
		}
		for _, must := range []string{"completeness.status", "raw_text", "as is"} {
			if !strings.Contains(tl.Description, must) {
				t.Errorf("%s: description lacks %q", name, must)
			}
		}
		if tl.Annotations == nil || !tl.Annotations.ReadOnlyHint {
			t.Errorf("%s must be annotated read-only", name)
		}
	}
	if len(got) != 4 {
		t.Errorf("tools = %d, want exactly 4 (no TFR tool, no route tool)", len(got))
	}
	// The NOTAM tool's description must warn about staging data and TFR limits.
	d := got["get_notams"].Description
	for _, must := range []string{"TFR", "applied by the FAA"} {
		if !strings.Contains(d, must) {
			t.Errorf("get_notams description lacks %q", must)
		}
	}
	// Input schemas expose the filters as properties.
	b, _ := json.Marshal(got["get_notams"].InputSchema)
	for _, p := range []string{"locations", "add_centers", "radius_nm", "effective_start", "free_text", "classification"} {
		if !strings.Contains(string(b), `"`+p+`"`) {
			t.Errorf("get_notams schema lacks %q: %s", p, b)
		}
	}
}

func TestWeatherReturnsEverythingWithProvenance(t *testing.T) {
	cs := connect(t, newUpstreams(t).deps(t, true, true))
	res, r := call(t, cs, "get_weather_advisories", nil)
	if res.IsError {
		t.Fatalf("error: %s", text(res))
	}
	if len(r.Provenance.Sources) != 4 {
		t.Fatalf("sources = %d", len(r.Provenance.Sources))
	}
	ids := []string{}
	total := 0
	for _, s := range r.Provenance.Sources {
		ids = append(ids, s.ID)
		total += s.RecordsReturned
		if s.ServedFrom != "live" || s.HTTPStatus != 200 || s.RecordsInFeed != s.RecordsReturned {
			t.Errorf("source %+v", s)
		}
	}
	// The envelope lists sources sorted by id (stable and deterministic).
	if strings.Join(ids, ",") != "awc.airsigmet,awc.cwa,awc.gairmet,awc.isigmet" {
		t.Errorf("source order = %v", ids)
	}
	var d struct {
		Filters struct {
			BySource map[string]string `json:"applied_by_source"`
			ByServer map[string]string `json:"applied_by_server"`
		} `json:"filters"`
		Kinds      []string          `json:"kinds_queried"`
		Advisories []json.RawMessage `json:"advisories"`
	}
	if err := json.Unmarshal(r.Data, &d); err != nil {
		t.Fatal(err)
	}
	if len(d.Advisories) != total || total != 1+34+10+2 {
		t.Errorf("advisories = %d, per-source total %d, want 47", len(d.Advisories), total)
	}
	if len(d.Filters.BySource) != 0 || len(d.Filters.ByServer) != 0 {
		t.Errorf("an unfiltered call must say no filter was applied: %+v", d.Filters)
	}
	if strings.Join(d.Kinds, ",") != "SIGMET,AIRMET,INTL_SIGMET,CWA" {
		t.Errorf("kinds = %v", d.Kinds)
	}
	if r.Disclaimer == "" || !strings.Contains(r.Disclaimer, "AS IS") || r.EvaluatedAt != "2026-11-23T14:00:00Z" {
		t.Errorf("disclaimer/evaluated_at: %q %q", r.Disclaimer, r.EvaluatedAt)
	}
	// Advisories follow the documented feed order, each feed in its own order.
	rank := map[string]int{"awc.airsigmet": 0, "awc.gairmet": 1, "awc.isigmet": 2, "awc.cwa": 3}
	last := -1
	for _, raw := range d.Advisories {
		var a struct {
			Source string `json:"source"`
		}
		json.Unmarshal(raw, &a)
		rk, ok := rank[a.Source]
		if !ok || rk < last {
			t.Fatalf("advisory from %q out of the documented feed order", a.Source)
		}
		last = rk
	}
	// Geometry is canonical GeoJSON in every record that has one.
	seen := map[string]int{}
	for _, raw := range d.Advisories {
		var a struct {
			Geometry *struct {
				Type        string          `json:"type"`
				Coordinates json.RawMessage `json:"coordinates"`
			} `json:"geometry"`
		}
		json.Unmarshal(raw, &a)
		if a.Geometry != nil {
			seen[a.Geometry.Type]++
			if len(a.Geometry.Coordinates) == 0 {
				t.Error("geometry without coordinates")
			}
		}
	}
	if seen["Polygon"] == 0 || seen["LineString"] == 0 {
		t.Errorf("expected canonical Polygon and LineString geometries, saw %v", seen)
	}
	if !r.hasIssue("UNDOCUMENTED_SOURCE", "") && r.Completeness.Status == "" {
		t.Error("completeness missing")
	}
}

func TestWeatherKindsAndHazardFilters(t *testing.T) {
	cs := connect(t, newUpstreams(t).deps(t, true, true))

	res, r := call(t, cs, "get_weather_advisories", map[string]any{"kinds": []string{"cwa"}})
	if res.IsError || len(r.Provenance.Sources) != 1 || r.Provenance.Sources[0].ID != "awc.cwa" {
		t.Fatalf("kinds=cwa: %+v err=%v", r.Provenance.Sources, res.IsError)
	}

	// An exact-match filter applied here is declared, and the omissions are visible.
	res, r = call(t, cs, "get_weather_advisories", map[string]any{"kinds": []string{"AIRMET"}, "hazard": "ice"})
	if res.IsError {
		t.Fatal(text(res))
	}
	s := r.Provenance.Sources[0]
	if s.RecordsInFeed != 34 || s.RecordsReturned == 0 || s.RecordsReturned >= 34 {
		t.Errorf("records in feed %d, returned %d", s.RecordsInFeed, s.RecordsReturned)
	}
	var d struct {
		Filters struct {
			ByServer map[string]string `json:"applied_by_server"`
		} `json:"filters"`
		Advisories []struct {
			Hazard string `json:"hazard"`
		} `json:"advisories"`
	}
	json.Unmarshal(r.Data, &d)
	if d.Filters.ByServer["hazard"] != "ice" || len(d.Advisories) != s.RecordsReturned {
		t.Errorf("filters = %+v, advisories = %d", d.Filters, len(d.Advisories))
	}
	for _, a := range d.Advisories {
		if !strings.EqualFold(a.Hazard, "ICE") {
			t.Errorf("hazard %q slipped through", a.Hazard)
		}
	}

	// No match: a genuine, correctly-typed empty list ([] not null), still attributable.
	_, r = call(t, cs, "get_weather_advisories", map[string]any{"kinds": []string{"CWA"}, "hazard": "NOSUCH"})
	if !strings.Contains(string(r.Data), `"advisories":[]`) || len(r.Provenance.Sources) != 1 {
		t.Errorf("data = %s", r.Data)
	}

	// Invalid kind is a request error that names the allowed values.
	res, _ = call(t, cs, "get_weather_advisories", map[string]any{"kinds": []string{"TFR"}})
	if !res.IsError || !strings.Contains(text(res), "SIGMET, AIRMET, INTL_SIGMET, CWA") {
		t.Errorf("invalid kind: isError=%v text=%q", res.IsError, text(res))
	}
}

func TestWeatherFailuresAreVisibleNotSilent(t *testing.T) {
	u := newUpstreams(t)
	cs := connect(t, u.deps(t, true, true))

	// One feed down: the rest is returned, the gap is a critical issue, status incomplete.
	u.awcFail["/gairmet"] = 503
	res, r := call(t, cs, "get_weather_advisories", nil)
	if res.IsError {
		t.Fatal("a partial failure still returns data")
	}
	if r.Completeness.Status != "incomplete" || !r.hasIssue("SOURCE_UNAVAILABLE", "awc.gairmet") {
		t.Errorf("status=%s issues=%+v", r.Completeness.Status, r.Completeness.Issues)
	}
	for _, i := range r.Completeness.Issues {
		if i.Code == "SOURCE_UNAVAILABLE" && (i.Severity != "critical" || !strings.Contains(i.Message, "HTTP 503")) {
			t.Errorf("issue = %+v", i)
		}
	}
	var failed bool
	for _, s := range r.Provenance.Sources {
		if s.ID == "awc.gairmet" {
			failed = true
			if s.HTTPStatus != 503 {
				t.Errorf("the failed source must still be attributed: %+v", s)
			}
		}
	}
	if !failed || len(r.Provenance.Sources) != 4 {
		t.Errorf("sources = %+v", r.Provenance.Sources)
	}

	// Everything down: an error result, but still the envelope, never an empty "none".
	for _, p := range []string{"/airsigmet", "/gairmet", "/isigmet", "/cwa"} {
		u.awcFail[p] = 500
	}
	res, r = call(t, cs, "get_weather_advisories", nil)
	if !res.IsError || r.Completeness.Status != "incomplete" || len(r.Provenance.Sources) != 4 {
		t.Errorf("isError=%v status=%s sources=%d", res.IsError, r.Completeness.Status, len(r.Provenance.Sources))
	}
	if strings.Contains(string(r.Data), `"advisories":[{`) {
		t.Error("no advisories can be present")
	}
}

func TestNotamsForwardFiltersAndSayWhoAppliedThem(t *testing.T) {
	u := newUpstreams(t)
	cs := connect(t, u.deps(t, true, false))

	res, r := call(t, cs, "get_notams", map[string]any{"locations": []string{"KIAD"}, "classification": "domestic", "free_text": "TWY"})
	if res.IsError {
		t.Fatal(text(res))
	}
	// One query for KIAD, then one for the center its NOTAMs list (ZDC), each
	// with the same filters.
	if len(u.notamQueries) != 2 {
		t.Fatalf("queries = %v", u.notamQueries)
	}
	for i, loc := range []string{"KIAD", "ZDC"} {
		q := u.notamQueries[i]
		if q["location"] != loc || q["classification"] != "DOMESTIC" || q["freeText"] != "TWY" {
			t.Errorf("forwarded[%d] = %v", i, q)
		}
	}
	if u.lastNotamHdr.Get("nmsResponseFormat") != "GEOJSON" || u.lastNotamHdr.Get("Authorization") != "Bearer tok" {
		t.Errorf("headers = %v", u.lastNotamHdr)
	}
	var d struct {
		Filters struct {
			BySource map[string]string `json:"applied_by_source"`
			ByServer map[string]string `json:"applied_by_server"`
		} `json:"filters"`
		NOTAMs []struct {
			ID      string `json:"id"`
			RawText string `json:"raw_text"`
		} `json:"notams"`
	}
	json.Unmarshal(r.Data, &d)
	if d.Filters.BySource["location"] != "KIAD,ZDC" || d.Filters.BySource["classification"] != "DOMESTIC" || len(d.Filters.ByServer) != 0 {
		t.Errorf("filters = %+v: every NOTAM filter belongs to the FAA, none to us", d.Filters)
	}
	if len(d.NOTAMs) != 16 {
		t.Errorf("notams = %d: nothing may be dropped or capped", len(d.NOTAMs))
	}
	for _, n := range d.NOTAMs {
		if n.RawText == "" {
			t.Errorf("%s: raw text lost", n.ID)
		}
	}
	// Not airworthy: the response says so and cannot assert completeness.
	if !r.hasIssue("NOT_AIRWORTHY", "") || r.Completeness.Status == "complete" || r.Airworthy == nil || *r.Airworthy {
		t.Errorf("status=%s issues=%+v", r.Completeness.Status, r.Completeness.Issues)
	}

	// The radius search and a time window forward in the API's own terms.
	call(t, cs, "get_notams", map[string]any{"latitude": 38.95, "longitude": -77.45, "radius_nm": 25.0, "effective_start": "2026-11-23T12:00:00-05:00", "effective_end": "2026-11-23T20:00:00Z"})
	q := u.lastNotamQuery
	if q["latitude"] != "38.95" || q["radius"] != "25" || q["effectiveStartDate"] != "2026-11-23T17:00:00Z" || q["effectiveEndDate"] != "2026-11-23T20:00:00Z" {
		t.Errorf("forwarded = %v", q)
	}

	// Airworthy: no mark.
	cs2 := connect(t, u.deps(t, true, true))
	_, r = call(t, cs2, "get_notams", map[string]any{"locations": []string{"KIAD"}})
	if r.hasIssue("NOT_AIRWORTHY", "") || r.Airworthy == nil || !*r.Airworthy {
		t.Error("an airworthy server must say airworthy: true and carry no mark")
	}
}

func TestNotamRequestErrorsAreCorrectableByTheAgent(t *testing.T) {
	u := newUpstreams(t)
	cs := connect(t, u.deps(t, true, true))
	cases := map[string]struct {
		args map[string]any
		want string
	}{
		"no filter":              {map[string]any{}, "at least one filter"},
		"lat without radius":     {map[string]any{"latitude": 38.9, "longitude": -77.4}, "together"},
		"radius too big":         {map[string]any{"latitude": 38.9, "longitude": -77.4, "radius_nm": 150.0}, "radius"},
		"short classification":   {map[string]any{"locations": []string{"KIAD"}, "classification": "DOM"}, "DOMESTIC, INTERNATIONAL"},
		"classification alone":   {map[string]any{"classification": "FDC"}, "classification alone"},
		"bad timestamp":          {map[string]any{"effective_start": "tomorrow", "effective_end": "2026-11-23T20:00:00Z"}, "effective_start must be an RFC 3339"},
		"bad last_updated":       {map[string]any{"last_updated": "yesterday"}, "last_updated must be an RFC 3339"},
		"window missing an end":  {map[string]any{"effective_start": "2026-11-23T12:00:00Z"}, "together"},
		"number without a place": {map[string]any{"notam_number": "08/224"}, "location or accountability"},
		"unknown feature":        {map[string]any{"locations": []string{"KIAD"}, "feature": "RUNWAY"}, "RWY"},
	}
	for name, tt := range cases {
		t.Run(name, func(t *testing.T) {
			res, _ := call(t, cs, "get_notams", tt.args)
			if !res.IsError || !strings.Contains(text(res), tt.want) {
				t.Errorf("isError=%v text=%q, want it to contain %q", res.IsError, text(res), tt.want)
			}
		})
	}
	if len(u.lastNotamQuery) != 0 {
		t.Errorf("invalid requests must never reach the FAA: %v", u.lastNotamQuery)
	}
}

func TestNotamsWhenTheSourceIsUnavailable(t *testing.T) {
	// Not configured: say so, mark incomplete, never an empty list that reads as "none".
	cs := connect(t, newUpstreams(t).deps(t, false, false))
	res, r := call(t, cs, "get_notams", map[string]any{"locations": []string{"KIAD"}})
	if !res.IsError || r.Completeness.Status != "incomplete" || len(r.Provenance.NotConsulted) != 1 || r.Provenance.NotConsulted[0].ID != "faa.nms.notam" {
		t.Errorf("isError=%v status=%s notConsulted=%+v", res.IsError, r.Completeness.Status, r.Provenance.NotConsulted)
	}
	if !r.hasIssue("SOURCE_NOT_ENABLED", "faa.nms.notam") || r.Completeness.Issues[0].Severity != "critical" {
		t.Errorf("issues = %+v", r.Completeness.Issues)
	}

	// Upstream failure.
	u := newUpstreams(t)
	u.nmsFail = 500
	cs = connect(t, u.deps(t, true, true))
	res, r = call(t, cs, "get_notams", map[string]any{"locations": []string{"KIAD"}})
	if !res.IsError || !r.hasIssue("SOURCE_UNAVAILABLE", "faa.nms.notam:KIAD") || r.Completeness.Status != "incomplete" {
		t.Errorf("isError=%v status=%s issues=%+v", res.IsError, r.Completeness.Status, r.Completeness.Issues)
	}
	if strings.Contains(text(res), "secret-0000000") {
		t.Error("a credential leaked into a tool response")
	}
}

func TestSourceStatus(t *testing.T) {
	u := newUpstreams(t)
	cs := connect(t, u.deps(t, true, false))
	res, r := call(t, cs, "source_status", nil)
	if res.IsError {
		t.Fatal(text(res))
	}
	var d struct {
		CheckedAt string `json:"checked_at"`
		Sources   []struct {
			ID, Environment, Status, Documentation string
			HTTPStatus                             int `json:"http_status"`
		} `json:"sources"`
	}
	json.Unmarshal(r.Data, &d)
	if len(d.Sources) != 5 || d.CheckedAt == "" {
		t.Fatalf("sources = %+v", d.Sources)
	}
	for _, s := range d.Sources {
		if s.Status != "ok" || s.HTTPStatus != 200 || s.Documentation != "documented" {
			t.Errorf("%+v", s)
		}
		if strings.HasPrefix(s.ID, "faa.nms") && s.Environment != "custom host" {
			t.Errorf("a host that is neither staging nor production must say so: %+v", s)
		}
	}
	if !r.hasIssue("NOT_AIRWORTHY", "") {
		t.Error("a server that is not airworthy must mark the status response too")
	}

	// A failing source is reported, not hidden.
	u.nmsFail = 503
	u.awcFail["/cwa"] = 500
	_, r = call(t, cs, "source_status", nil)
	json.Unmarshal(r.Data, &d)
	bad := map[string]bool{}
	for _, s := range d.Sources {
		if s.Status == "error" {
			bad[s.ID] = true
		}
	}
	if !bad["awc.cwa"] || !bad["faa.nms.notam"] || r.Completeness.Status != "incomplete" {
		t.Errorf("bad=%v status=%s", bad, r.Completeness.Status)
	}

	// Unconfigured NOTAM source is reported as such.
	cs2 := connect(t, newUpstreams(t).deps(t, false, false))
	_, r = call(t, cs2, "source_status", nil)
	json.Unmarshal(r.Data, &d)
	last := d.Sources[len(d.Sources)-1]
	if last.ID != "faa.nms.notam" || last.Status != "not_enabled" || len(r.Provenance.NotConsulted) != 1 {
		t.Errorf("last = %+v", last)
	}
}

func TestNewRequiresAnAWCClient(t *testing.T) {
	if _, err := New(Deps{}); err == nil {
		t.Error("expected an error")
	}
}

// A caller-chosen limit is allowed, but it must never be silent: the response
// says how many items were omitted and cannot read as complete.
func TestCallerChosenLimitIsReportedNotSilent(t *testing.T) {
	u := newUpstreams(t)
	cs := connect(t, u.deps(t, true, true))

	res, r := call(t, cs, "get_weather_advisories", map[string]any{"limit": 5})
	if res.IsError {
		t.Fatal(text(res))
	}
	var d struct {
		Advisories []struct {
			Source string `json:"source"`
		} `json:"advisories"`
	}
	json.Unmarshal(r.Data, &d)
	if len(d.Advisories) != 5 {
		t.Fatalf("advisories = %d", len(d.Advisories))
	}
	var trunc bool
	for _, i := range r.Completeness.Issues {
		if i.Code == "RESULT_TRUNCATED" {
			trunc = i.Severity == "warning" && strings.Contains(i.Message, "42 of 47 items were omitted") && strings.Contains(i.Message, "limit=5")
		}
	}
	if !trunc || r.Completeness.Status == "complete" {
		t.Errorf("truncation must be reported and the response not complete: status=%s issues=%+v", r.Completeness.Status, r.Completeness.Issues)
	}
	sum := 0
	for _, s := range r.Provenance.Sources {
		sum += s.RecordsReturned
		if s.RecordsInFeed == 0 {
			t.Errorf("the feed size must still be reported: %+v", s)
		}
	}
	if sum != 5 {
		t.Errorf("per-source records_returned add up to %d, want 5", sum)
	}

	// A limit that omits nothing is not reported as truncation.
	_, r = call(t, cs, "get_weather_advisories", map[string]any{"limit": 1000})
	if r.hasIssue("RESULT_TRUNCATED", "") {
		t.Error("nothing was omitted")
	}

	// NOTAMs: same contract.
	res, r = call(t, cs, "get_notams", map[string]any{"locations": []string{"KIAD"}, "limit": 3})
	var nd struct {
		NOTAMs []json.RawMessage `json:"notams"`
	}
	json.Unmarshal(r.Data, &nd)
	if res.IsError || len(nd.NOTAMs) != 3 || !r.hasIssue("RESULT_TRUNCATED", "") || r.Provenance.Sources[0].RecordsReturned != 3 || r.Provenance.Sources[0].RecordsInFeed != 16 {
		t.Errorf("notams=%d issues=%+v source=%+v", len(nd.NOTAMs), r.Completeness.Issues, r.Provenance.Sources)
	}

	// Invalid limits are correctable request errors.
	for _, bad := range []int{0, -1, 1000000} {
		for _, tool := range []string{"get_weather_advisories", "get_notams"} {
			args := map[string]any{"limit": bad}
			if tool == "get_notams" {
				args["locations"] = []string{"KIAD"}
			}
			if res, _ := call(t, cs, tool, args); !res.IsError || !strings.Contains(text(res), "limit must be between") {
				t.Errorf("%s limit=%d: isError=%v %q", tool, bad, res.IsError, text(res))
			}
		}
	}
}

func TestGeometryCanBeOmittedOnRequestAndSaysSo(t *testing.T) {
	cs := connect(t, newUpstreams(t).deps(t, true, true))
	hasGeom := func(data json.RawMessage, key string) (withGeom, total int, omitted []string) {
		var d struct {
			Omitted []string                   `json:"omitted_fields"`
			Items   map[string]json.RawMessage `json:"-"`
		}
		json.Unmarshal(data, &d)
		var raw map[string]json.RawMessage
		json.Unmarshal(data, &raw)
		var items []map[string]json.RawMessage
		json.Unmarshal(raw[key], &items)
		for _, it := range items {
			total++
			if _, ok := it["geometry"]; ok {
				withGeom++
			}
			if _, ok := it["geometry_derivation"]; ok {
				withGeom++
			}
		}
		return withGeom, total, d.Omitted
	}

	_, r := call(t, cs, "get_weather_advisories", map[string]any{"kinds": []string{"AIRMET"}})
	if g, n, om := hasGeom(r.Data, "advisories"); g == 0 || n != 34 || len(om) != 0 {
		t.Errorf("default must include geometry: geom=%d total=%d omitted=%v", g, n, om)
	}
	_, r = call(t, cs, "get_weather_advisories", map[string]any{"kinds": []string{"AIRMET"}, "include_geometry": false})
	if g, n, om := hasGeom(r.Data, "advisories"); g != 0 || n != 34 || len(om) != 1 || om[0] != "geometry" {
		t.Errorf("geometry must be omitted and declared: geom=%d total=%d omitted=%v", g, n, om)
	}
	_, r = call(t, cs, "get_notams", map[string]any{"locations": []string{"KIAD"}})
	if g, _, om := hasGeom(r.Data, "notams"); g == 0 || len(om) != 0 {
		t.Errorf("default notams must include geometry: %d %v", g, om)
	}
	_, r = call(t, cs, "get_notams", map[string]any{"locations": []string{"KIAD"}, "include_geometry": false})
	if g, n, om := hasGeom(r.Data, "notams"); g != 0 || n != 16 || len(om) != 1 || om[0] != "geometry" {
		t.Errorf("geometry must be omitted and declared: geom=%d total=%d omitted=%v", g, n, om)
	}
	// Omitting geometry drops nothing else: raw text and the record count are intact.
	if !strings.Contains(string(r.Data), `"raw_text"`) {
		t.Error("raw text must survive")
	}
}

// Unless the server is airworthy, every response from every tool, including
// error envelopes, says airworthy: false and carries NOT_AIRWORTHY, and the
// descriptions say so before anything is called.
func TestAirworthyIsOnEveryResponse(t *testing.T) {
	for _, airworthy := range []bool{false, true} {
		u := newUpstreams(t)
		cs := connect(t, u.deps(t, true, airworthy))
		off := connect(t, u.deps(t, false, airworthy)) // NOTAMs not enabled
		calls := []struct {
			cs   *mcp.ClientSession
			name string
			args map[string]any
		}{
			{cs, "get_weather_advisories", nil},
			{cs, "get_notams", map[string]any{"locations": []string{"KIAD"}}},
			{cs, "source_status", nil},
			{off, "get_notams", map[string]any{"locations": []string{"KIAD"}}},
		}
		for _, c := range calls {
			_, r := call(t, c.cs, c.name, c.args)
			if r.Airworthy == nil || *r.Airworthy != airworthy {
				t.Errorf("airworthy=%v %s: envelope airworthy = %v", airworthy, c.name, r.Airworthy)
			}
			if got := r.hasIssue("NOT_AIRWORTHY", ""); got == airworthy {
				t.Errorf("airworthy=%v %s: NOT_AIRWORTHY present=%v", airworthy, c.name, got)
			}
			if !airworthy && r.Completeness.Status == "complete" {
				t.Errorf("%s: a response that is not airworthy must never read as complete", c.name)
			}
			for _, i := range r.Completeness.Issues {
				if i.Code == "NOT_AIRWORTHY" && (i.Severity != "warning" || !strings.Contains(i.Message, "Do not use it for flight")) {
					t.Errorf("issue = %+v", i)
				}
			}
		}
		tools, _ := cs.ListTools(context.Background(), nil)
		for _, tl := range tools.Tools {
			if got := strings.HasPrefix(tl.Description, "NOT AIRWORTHY:"); got == airworthy {
				t.Errorf("airworthy=%v %s: description prefixed=%v", airworthy, tl.Name, got)
			}
		}
		if got := strings.HasPrefix(cs.InitializeResult().Instructions, "NOT AIRWORTHY:"); got == airworthy {
			t.Errorf("airworthy=%v: server instructions prefixed=%v", airworthy, got)
		}
	}
}

// notamsReply is the part of get_notams data the location tests inspect.
type notamsReply struct {
	Filters struct {
		BySource map[string]string `json:"applied_by_source"`
	} `json:"filters"`
	Locations *struct {
		Requested []string `json:"requested"`
		Added     []struct {
			Location string   `json:"location"`
			ListedOn []string `json:"listed_on"`
			Refs     []string `json:"refs"`
		} `json:"added"`
		Note string `json:"note"`
	} `json:"locations"`
	NOTAMs []struct {
		ID          string   `json:"id"`
		ReturnedFor []string `json:"returned_for"`
	} `json:"notams"`
}

func (u *upstreams) queriedLocations() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	var out []string
	for _, q := range u.notamQueries {
		out = append(out, q["location"])
	}
	sort.Strings(out)
	return out
}

func TestNotamLocationsAreMergedAndTheirCenterAdded(t *testing.T) {
	u := newUpstreams(t)
	cs := connect(t, u.deps(t, true, false))

	// Every fixture NOTAM lists ZDC as its center, and both airports return
	// the same records: one copy each, returned for all three queries.
	res, r := call(t, cs, "get_notams", map[string]any{"locations": []string{"KIAD", "kiad", " KDCA "}, "include_geometry": false})
	if res.IsError {
		t.Fatal(text(res))
	}
	if got := u.queriedLocations(); strings.Join(got, ",") != "KDCA,KIAD,ZDC" {
		t.Errorf("queried = %v: one query per distinct identifier, plus the center", got)
	}
	var d notamsReply
	json.Unmarshal(r.Data, &d)
	if d.Locations == nil || strings.Join(d.Locations.Requested, ",") != "KIAD,KDCA" || d.Locations.Note != centersNotInferred {
		t.Fatalf("locations = %+v", d.Locations)
	}
	if len(d.Locations.Added) != 1 || d.Locations.Added[0].Location != "ZDC" ||
		strings.Join(d.Locations.Added[0].ListedOn, ",") != "KIAD,KDCA" || len(d.Locations.Added[0].Refs) == 0 {
		t.Errorf("added = %+v", d.Locations.Added)
	}
	if d.Filters.BySource["location"] != "KIAD,KDCA,ZDC" {
		t.Errorf("applied_by_source = %v", d.Filters.BySource)
	}
	if len(d.NOTAMs) != 16 {
		t.Errorf("notams = %d, want 16: a NOTAM returned by several queries appears once", len(d.NOTAMs))
	}
	for _, n := range d.NOTAMs {
		if strings.Join(n.ReturnedFor, ",") != "KIAD,KDCA,ZDC" {
			t.Errorf("%s returned_for = %v", n.ID, n.ReturnedFor)
		}
	}
	ids := map[string]int{}
	for _, s := range r.Provenance.Sources {
		ids[s.ID] = s.RecordsReturned
	}
	for _, id := range []string{"faa.nms.notam:KIAD", "faa.nms.notam:KDCA", "faa.nms.notam:ZDC"} {
		if ids[id] != 16 {
			t.Errorf("source %s records_returned = %d (sources %v)", id, ids[id], ids)
		}
	}
	if r.hasIssue("CENTER_NOT_IMPLIED", "") {
		t.Error("every location listed a center")
	}
}

func TestNotamCentersAreOnlyAddedWhenWantedAndNew(t *testing.T) {
	// Turned off: only what was asked for.
	u := newUpstreams(t)
	cs := connect(t, u.deps(t, true, false))
	_, r := call(t, cs, "get_notams", map[string]any{"locations": []string{"KIAD"}, "add_centers": false})
	var d notamsReply
	json.Unmarshal(r.Data, &d)
	if got := u.queriedLocations(); strings.Join(got, ",") != "KIAD" || len(d.Locations.Added) != 0 {
		t.Errorf("queried = %v added = %+v", got, d.Locations.Added)
	}

	// Already listed, in the other spelling: not asked for twice.
	u = newUpstreams(t)
	cs = connect(t, u.deps(t, true, false))
	_, r = call(t, cs, "get_notams", map[string]any{"locations": []string{"KIAD", "KZDC"}})
	d = notamsReply{}
	json.Unmarshal(r.Data, &d)
	if got := u.queriedLocations(); strings.Join(got, ",") != "KIAD,KZDC" || len(d.Locations.Added) != 0 {
		t.Errorf("queried = %v added = %+v", got, d.Locations.Added)
	}

	// No locations: no center logic at all.
	u = newUpstreams(t)
	cs = connect(t, u.deps(t, true, false))
	_, r = call(t, cs, "get_notams", map[string]any{"latitude": 38.95, "longitude": -77.45, "radius_nm": 5.0})
	d = notamsReply{}
	json.Unmarshal(r.Data, &d)
	if got := u.queriedLocations(); len(got) != 1 || got[0] != "" || d.Locations != nil || r.Provenance.Sources[0].ID != "faa.nms.notam" {
		t.Errorf("queried = %v locations = %+v sources = %+v", got, d.Locations, r.Provenance.Sources)
	}
}

func TestNotamCenterThatCannotBeImpliedIsAWarning(t *testing.T) {
	u := newUpstreams(t)
	noFIR := regexp.MustCompile(`"affectedFir":\s*"[^"]*",?`).ReplaceAll(fixture(t, "nms", "notams_sample.json"), nil)
	u.nmsBodyFor["L45"] = noFIR
	cs := connect(t, u.deps(t, true, true))
	res, r := call(t, cs, "get_notams", map[string]any{"locations": []string{"L45"}})
	if res.IsError {
		t.Fatal(text(res))
	}
	if !r.hasIssue("CENTER_NOT_IMPLIED", "faa.nms.notam:L45") || r.Completeness.Status != "degraded" {
		t.Errorf("status=%s issues=%+v: a location whose center cannot be found must say so", r.Completeness.Status, r.Completeness.Issues)
	}
	if got := u.queriedLocations(); strings.Join(got, ",") != "L45" {
		t.Errorf("queried = %v", got)
	}
}

func TestOneFailedLocationMakesTheResponseIncompleteNotEmpty(t *testing.T) {
	u := newUpstreams(t)
	u.nmsFailFor["KDCA"] = 503
	cs := connect(t, u.deps(t, true, true))
	res, r := call(t, cs, "get_notams", map[string]any{"locations": []string{"KIAD", "KDCA"}})
	if res.IsError {
		t.Fatalf("some data was obtained, so this is not an error: %s", text(res))
	}
	var d notamsReply
	json.Unmarshal(r.Data, &d)
	if r.Completeness.Status != "incomplete" || !r.hasIssue("SOURCE_UNAVAILABLE", "faa.nms.notam:KDCA") || len(d.NOTAMs) != 16 {
		t.Errorf("status=%s notams=%d issues=%+v", r.Completeness.Status, len(d.NOTAMs), r.Completeness.Issues)
	}
	for _, n := range d.NOTAMs {
		for _, l := range n.ReturnedFor {
			if l == "KDCA" {
				t.Fatalf("%s claims to come from the failed query", n.ID)
			}
		}
	}
}

func TestNotamLocationsAreValidated(t *testing.T) {
	u := newUpstreams(t)
	cs := connect(t, u.deps(t, true, true))
	many := make([]string, maxLocations+1)
	for i := range many {
		many[i] = fmt.Sprintf("K%03d", i)
	}
	for name, args := range map[string]map[string]any{
		"too many": {"locations": many},
		"empty":    {"locations": []string{"KIAD", " "}},
	} {
		res, _ := call(t, cs, "get_notams", args)
		if !res.IsError {
			t.Errorf("%s: want a correctable error", name)
		}
	}
	if len(u.queriedLocations()) != 0 {
		t.Error("nothing may be sent for a request that is refused")
	}
}

// The parameters both data tools share must be described identically, so an
// agent reads one contract (struct tags cannot share a constant).
func TestSharedParametersAreDescribedTheSame(t *testing.T) {
	u := newUpstreams(t)
	cs := connect(t, u.deps(t, true, false))
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	desc := map[string]map[string]string{}
	for _, tool := range res.Tools {
		b, _ := json.Marshal(tool.InputSchema)
		var sch struct {
			Properties map[string]struct {
				Description string `json:"description"`
			} `json:"properties"`
		}
		json.Unmarshal(b, &sch)
		for name, p := range sch.Properties {
			if desc[name] == nil {
				desc[name] = map[string]string{}
			}
			desc[name][tool.Name] = p.Description
		}
	}
	for _, name := range []string{"limit", "include_geometry"} {
		n, w := desc[name]["get_notams"], desc[name]["get_weather_advisories"]
		if n == "" || n != w {
			t.Errorf("%s is described differently:\n get_notams: %q\n get_weather_advisories: %q", name, n, w)
		}
	}
}

func TestNotamKeyKeepsDifferentStubsApart(t *testing.T) {
	// A stub's id is only its position in one query, so two different records
	// that failed to parse in two location queries can share it. They must not
	// merge into one (that would drop a record).
	a := model.Notam{ID: "faa.nms.notam#3", SourceRecord: []byte(`{"n":1}`)}
	b := model.Notam{ID: "faa.nms.notam#3", SourceRecord: []byte(`{"n":2}`)}
	if notamKey(a) == notamKey(b) {
		t.Error("different source records under the same stub id share a key")
	}
	if notamKey(a) != notamKey(model.Notam{ID: a.ID, SourceRecord: []byte(`{"n":1}`)}) {
		t.Error("the same record must keep the same key")
	}
}
