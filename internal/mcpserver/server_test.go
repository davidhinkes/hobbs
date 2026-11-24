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
	for _, name := range []string{"get_flight_briefing_data", "source_status"} {
		tl := got[name]
		if tl == nil {
			t.Fatalf("missing tool %s (have %v)", name, got)
		}
		for _, must := range []string{"completeness.status", "raw_text", "as is", "icao_notam, us_notam, advisory_text, metar or taf"} {
			if !strings.Contains(tl.Description, must) {
				t.Errorf("%s: description lacks %q", name, must)
			}
		}
		if tl.Annotations == nil || !tl.Annotations.ReadOnlyHint {
			t.Errorf("%s must be annotated read-only", name)
		}
	}
	if len(got) != 2 {
		t.Errorf("tools = %d, want exactly 2: get_flight_briefing_data and source_status (no TFR tool, no route tool)", len(got))
	}
	// The data tool's description must warn about TFR limits and say what it does not infer.
	d := got["get_flight_briefing_data"].Description
	for _, must := range []string{"TFR", "never inferred", "fltCat", "not computed by hobbs", "full national sets"} {
		if !strings.Contains(d, must) {
			t.Errorf("get_flight_briefing_data description lacks %q", must)
		}
	}
	// The input schema exposes only places, include and include_geometry.
	b, _ := json.Marshal(got["get_flight_briefing_data"].InputSchema)
	var sch struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	json.Unmarshal(b, &sch)
	var props []string
	for p := range sch.Properties {
		props = append(props, p)
	}
	sort.Strings(props)
	if strings.Join(props, ",") != "airports,centers,include,include_geometry" {
		t.Errorf("get_flight_briefing_data properties = %v", props)
	}
	// The server instructions name the data tool and METARs/TAFs.
	in := cs.InitializeResult().Instructions
	for _, must := range []string{"get_flight_briefing_data", "METARs", "TAFs"} {
		if !strings.Contains(in, must) {
			t.Errorf("server instructions lack %q", must)
		}
	}
}

// advisoryArgs asks get_flight_briefing_data for the four advisory sets only.
func advisoryArgs(extra map[string]any) map[string]any {
	args := map[string]any{"airports": []string{"KSMO"}, "include": []string{"sigmets", "airmets", "intl_sigmets", "cwas"}}
	for k, v := range extra {
		args[k] = v
	}
	return args
}

// advisorySublists are the advisory sublists, in output order, with the
// source each comes from.
var advisorySublists = []struct{ key, source string }{
	{"sigmets", "awc.airsigmet"}, {"airmets", "awc.gairmet"}, {"intl_sigmets", "awc.isigmet"}, {"cwas", "awc.cwa"},
}

func TestWeatherReturnsEverythingWithProvenance(t *testing.T) {
	cs := connect(t, newUpstreams(t).deps(t, true, true))
	res, r := call(t, cs, "get_flight_briefing_data", advisoryArgs(map[string]any{"include_geometry": true}))
	if res.IsError {
		t.Fatalf("error: %s", text(res))
	}
	if len(r.Provenance.Sources) != 4 {
		t.Fatalf("sources = %d", len(r.Provenance.Sources))
	}
	ids := []string{}
	total := 0
	perSource := map[string]int{}
	for _, s := range r.Provenance.Sources {
		ids = append(ids, s.ID)
		total += s.RecordsReturned
		perSource[s.ID] = s.RecordsReturned
		if s.ServedFrom != "live" || s.HTTPStatus != 200 || s.RecordsInFeed != s.RecordsReturned {
			t.Errorf("source %+v", s)
		}
	}
	// The envelope lists sources sorted by id (stable and deterministic).
	if strings.Join(ids, ",") != "awc.airsigmet,awc.cwa,awc.gairmet,awc.isigmet" {
		t.Errorf("source order = %v", ids)
	}
	b := decodeBriefing(t, r)
	n := 0
	seen := map[string]int{}
	for _, sl := range advisorySublists {
		items := b.items(t, sl.key)
		n += len(items)
		if len(items) != perSource[sl.source] {
			t.Errorf("%s = %d items, %s records_returned = %d", sl.key, len(items), sl.source, perSource[sl.source])
		}
		for _, it := range items {
			// Each sublist holds only its own feed's records.
			var src string
			json.Unmarshal(it["source"], &src)
			if src != sl.source {
				t.Errorf("%s holds a record from %q", sl.key, src)
			}
			// Geometry is canonical GeoJSON in every record that has one.
			var g *struct {
				Type        string          `json:"type"`
				Coordinates json.RawMessage `json:"coordinates"`
			}
			if raw, ok := it["geometry"]; ok {
				json.Unmarshal(raw, &g)
			}
			if g != nil {
				seen[g.Type]++
				if len(g.Coordinates) == 0 {
					t.Error("geometry without coordinates")
				}
			}
		}
	}
	if n != total || total != 1+34+10+2 {
		t.Errorf("advisories = %d, per-source total %d, want 47", n, total)
	}
	for _, tr := range b.Translation {
		if len(tr.AppliedBySource) != 0 {
			t.Errorf("an advisory set is not filtered: %+v", tr)
		}
	}
	if r.Disclaimer == "" || !strings.Contains(r.Disclaimer, "AS IS") || r.EvaluatedAt != "2026-11-23T14:00:00Z" {
		t.Errorf("disclaimer/evaluated_at: %q %q", r.Disclaimer, r.EvaluatedAt)
	}
	if seen["Polygon"] == 0 || seen["LineString"] == 0 {
		t.Errorf("expected canonical Polygon and LineString geometries, saw %v", seen)
	}
	if r.Completeness.Status == "" {
		t.Error("completeness missing")
	}
}

func TestWeatherFailuresAreVisibleNotSilent(t *testing.T) {
	u := newUpstreams(t)
	cs := connect(t, u.deps(t, true, true))

	// One feed down: the rest is returned, the gap is a critical issue, status incomplete.
	u.awcFail["/gairmet"] = 503
	res, r := call(t, cs, "get_flight_briefing_data", advisoryArgs(nil))
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
	b := decodeBriefing(t, r)
	if a := b.items(t, "airmets"); a == nil || len(a) != 0 {
		t.Errorf("airmets = %v, want []", a)
	}
	if len(b.items(t, "intl_sigmets")) == 0 {
		t.Error("the other feeds must still be returned")
	}

	// Everything down: an error result, but still the envelope, never an empty "none".
	for _, p := range []string{"/airsigmet", "/gairmet", "/isigmet", "/cwa"} {
		u.awcFail[p] = 500
	}
	res, r = call(t, cs, "get_flight_briefing_data", advisoryArgs(nil))
	if !res.IsError || r.Completeness.Status != "incomplete" || len(r.Provenance.Sources) != 4 {
		t.Errorf("isError=%v status=%s sources=%d", res.IsError, r.Completeness.Status, len(r.Provenance.Sources))
	}
	b = decodeBriefing(t, r)
	for _, sl := range advisorySublists {
		if n := len(b.items(t, sl.key)); n != 0 {
			t.Errorf("%s = %d items: no advisories can be present", sl.key, n)
		}
	}
}

func TestNotamsWhenTheSourceIsUnavailable(t *testing.T) {
	// Not configured: say so, mark incomplete, never an empty list that reads as "none".
	notams := map[string]any{"airports": []string{"KIAD"}, "include": []string{"notams"}}
	cs := connect(t, newUpstreams(t).deps(t, false, false))
	res, r := call(t, cs, "get_flight_briefing_data", notams)
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
	res, r = call(t, cs, "get_flight_briefing_data", notams)
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
			RecordsInFeed                          int `json:"records_in_feed"`
		} `json:"sources"`
	}
	json.Unmarshal(r.Data, &d)
	var ids []string
	for _, s := range d.Sources {
		ids = append(ids, s.ID)
	}
	// The four advisory feeds, then METAR and TAF, then NMS.
	if strings.Join(ids, ",") != "awc.airsigmet,awc.gairmet,awc.isigmet,awc.cwa,awc.metar,awc.taf,faa.nms.notam" || d.CheckedAt == "" {
		t.Fatalf("sources = %+v", d.Sources)
	}
	for _, s := range d.Sources {
		if s.Status != "ok" || s.HTTPStatus != 200 || s.Documentation != "documented" {
			t.Errorf("%+v", s)
		}
		if strings.HasPrefix(s.ID, "faa.nms") && s.Environment != "custom host" {
			t.Errorf("a host that is neither staging nor production must say so: %+v", s)
		}
		if (s.ID == "awc.metar" || s.ID == "awc.taf") && (s.Environment != "n/a" || s.RecordsInFeed == 0) {
			t.Errorf("%+v", s)
		}
	}
	// One request each for METAR and TAF, for the probe station.
	for _, p := range []string{"/metar", "/taf"} {
		if u.awcCalls[p] != 1 || !strings.Contains(u.awcQueries[p], "ids=KLAX&") {
			t.Errorf("%s: calls = %d query = %q", p, u.awcCalls[p], u.awcQueries[p])
		}
	}
	provIDs := sourceIDs(r)
	if !provIDs["awc.metar"] || !provIDs["awc.taf"] || len(r.Provenance.Sources) != 7 {
		t.Errorf("provenance sources = %v", provIDs)
	}
	if !r.hasIssue("NOT_AIRWORTHY", "") {
		t.Error("a server that is not airworthy must mark the status response too")
	}

	// A failing source is reported, not hidden.
	u.nmsFail = 503
	u.awcFail["/cwa"] = 500
	u.awcFail["/taf"] = 503
	_, r = call(t, cs, "source_status", nil)
	json.Unmarshal(r.Data, &d)
	bad := map[string]bool{}
	for _, s := range d.Sources {
		if s.Status == "error" {
			bad[s.ID] = true
		}
	}
	if len(bad) != 3 || !bad["awc.cwa"] || !bad["awc.taf"] || !bad["faa.nms.notam"] || r.Completeness.Status != "incomplete" || !r.hasIssue("SOURCE_UNAVAILABLE", "awc.taf") {
		t.Errorf("bad=%v status=%s", bad, r.Completeness.Status)
	}

	// Unconfigured NOTAM source is reported as such.
	cs2 := connect(t, newUpstreams(t).deps(t, false, false))
	_, r = call(t, cs2, "source_status", nil)
	json.Unmarshal(r.Data, &d)
	last := d.Sources[len(d.Sources)-1]
	if len(d.Sources) != 7 || last.ID != "faa.nms.notam" || last.Status != "not_enabled" || len(r.Provenance.NotConsulted) != 1 {
		t.Errorf("last = %+v", last)
	}
}

// A METAR probe that comes back empty is the anomaly the check exists to
// show: its STATION_NO_DATA warning reaches the response, not only "ok".
func TestSourceStatusKeepsProbeIssues(t *testing.T) {
	u := newUpstreams(t)
	u.awcFail["/metar"] = 204
	cs := connect(t, u.deps(t, true, false))
	res, r := call(t, cs, "source_status", nil)
	if res.IsError {
		t.Fatal(text(res))
	}
	if !r.hasIssue("STATION_NO_DATA", "awc.metar") {
		t.Errorf("no STATION_NO_DATA issue for awc.metar: %+v", r.Completeness)
	}
}

func TestNewRequiresAnAWCClient(t *testing.T) {
	if _, err := New(Deps{}); err == nil {
		t.Error("expected an error")
	}
}

func TestGeometryCanBeOmittedOnRequestAndSaysSo(t *testing.T) {
	cs := connect(t, newUpstreams(t).deps(t, true, true))
	hasGeom := func(data json.RawMessage, key string) (withGeom, total int, omitted []string) {
		var d struct {
			Omitted []string `json:"omitted_fields"`
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

	airmets := map[string]any{"airports": []string{"KSMO"}, "include": []string{"airmets"}}
	_, r := call(t, cs, "get_flight_briefing_data", airmets)
	if g, n, om := hasGeom(r.Data, "airmets"); g != 0 || n != 34 || len(om) != 1 || om[0] != "geometry" {
		t.Errorf("by default geometry must be omitted and declared: geom=%d total=%d omitted=%v", g, n, om)
	}
	airmets["include_geometry"] = true
	_, r = call(t, cs, "get_flight_briefing_data", airmets)
	if g, n, om := hasGeom(r.Data, "airmets"); g == 0 || n != 34 || om == nil || len(om) != 0 {
		t.Errorf("geometry must be included on request: geom=%d total=%d omitted=%v", g, n, om)
	}
	notams := map[string]any{"airports": []string{"KIAD"}, "include": []string{"notams"}, "include_geometry": true}
	_, r = call(t, cs, "get_flight_briefing_data", notams)
	if g, _, om := hasGeom(r.Data, "notams"); g == 0 || len(om) != 0 {
		t.Errorf("notams must include geometry on request: %d %v", g, om)
	}
	delete(notams, "include_geometry")
	_, r = call(t, cs, "get_flight_briefing_data", notams)
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
			{cs, "get_flight_briefing_data", map[string]any{"airports": []string{"KIAD"}}},
			{cs, "source_status", nil},
			{off, "get_flight_briefing_data", map[string]any{"airports": []string{"KIAD"}, "include": []string{"notams"}}}, // an error envelope
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

// notamsReply is the part of get_flight_briefing_data data the NOTAM location
// tests inspect.
type notamsReply struct {
	Translation []translationEntry `json:"translation"`
	Locations   *struct {
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
		RawText     string   `json:"raw_text"`
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
	res, r := call(t, cs, "get_flight_briefing_data", map[string]any{"airports": []string{"KIAD", "kiad", " KDCA "}, "include": []string{"notams"}})
	if res.IsError {
		t.Fatal(text(res))
	}
	if got := u.queriedLocations(); strings.Join(got, ",") != "KDCA,KIAD,ZDC" {
		t.Errorf("queried = %v: one query per distinct identifier, plus the center", got)
	}
	if u.lastNotamHdr.Get("nmsResponseFormat") != "GEOJSON" || u.lastNotamHdr.Get("Authorization") != "Bearer tok" {
		t.Errorf("headers = %v", u.lastNotamHdr)
	}
	for _, q := range u.notamQueries {
		if len(q) != 1 {
			t.Errorf("the FAA is asked by location only: %v", q)
		}
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
	if len(d.Translation) != 1 || d.Translation[0].AppliedBySource["location"] != "KIAD,KDCA" {
		t.Errorf("translation = %+v", d.Translation)
	}
	if len(d.NOTAMs) != 16 {
		t.Errorf("notams = %d, want 16: a NOTAM returned by several queries appears once", len(d.NOTAMs))
	}
	for _, n := range d.NOTAMs {
		if strings.Join(n.ReturnedFor, ",") != "KIAD,KDCA,ZDC" {
			t.Errorf("%s returned_for = %v", n.ID, n.ReturnedFor)
		}
		if n.RawText == "" {
			t.Errorf("%s: raw text lost", n.ID)
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

	// A center already listed, in the other spelling, is not asked for twice.
	u = newUpstreams(t)
	cs = connect(t, u.deps(t, true, false))
	_, r = call(t, cs, "get_flight_briefing_data", map[string]any{"airports": []string{"KIAD"}, "centers": []string{"KZDC"}, "include": []string{"notams"}})
	d = notamsReply{}
	json.Unmarshal(r.Data, &d)
	if got := u.queriedLocations(); strings.Join(got, ",") != "KIAD,ZDC" || d.Locations == nil || len(d.Locations.Added) != 0 {
		t.Errorf("queried = %v locations = %+v", got, d.Locations)
	}
}

func TestNotamCenterThatCannotBeImpliedIsAWarning(t *testing.T) {
	u := newUpstreams(t)
	noFIR := regexp.MustCompile(`"affectedFir":\s*"[^"]*",?`).ReplaceAll(fixture(t, "nms", "notams_sample.json"), nil)
	u.nmsBodyFor["KL45"] = noFIR
	cs := connect(t, u.deps(t, true, true))
	res, r := call(t, cs, "get_flight_briefing_data", map[string]any{"airports": []string{"KL45"}, "include": []string{"notams"}})
	if res.IsError {
		t.Fatal(text(res))
	}
	if !r.hasIssue("CENTER_NOT_IMPLIED", "faa.nms.notam:KL45") || r.Completeness.Status != "degraded" {
		t.Errorf("status=%s issues=%+v: a location whose center cannot be found must say so", r.Completeness.Status, r.Completeness.Issues)
	}
	if got := u.queriedLocations(); strings.Join(got, ",") != "KL45" {
		t.Errorf("queried = %v", got)
	}
}

func TestOneFailedLocationMakesTheResponseIncompleteNotEmpty(t *testing.T) {
	u := newUpstreams(t)
	u.nmsFailFor["KDCA"] = 503
	cs := connect(t, u.deps(t, true, true))
	res, r := call(t, cs, "get_flight_briefing_data", map[string]any{"airports": []string{"KIAD", "KDCA"}, "include": []string{"notams"}})
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
		"too many":      {"airports": many},
		"empty":         {"airports": []string{"KIAD", " "}},
		"3-letter":      {"airports": []string{"IAD"}},
		"selector":      {"airports": []string{"@VA"}},
		"bad center":    {"airports": []string{"KIAD"}, "centers": []string{"DCA"}},
		"too many both": {"airports": many[:9], "centers": []string{"ZDC", "ZNY"}},
	} {
		res, _ := call(t, cs, "get_flight_briefing_data", args)
		if !res.IsError {
			t.Errorf("%s: want a correctable error", name)
		}
	}
	if len(u.queriedLocations()) != 0 || len(u.awcCalls) != 0 {
		t.Error("nothing may be sent for a request that is refused")
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
