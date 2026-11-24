package mcpserver

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// briefingSublists are the data keys of get_flight_briefing_data, in order.
var briefingSublists = []string{"notams", "metars", "tafs", "sigmets", "airmets", "intl_sigmets", "cwas"}

// briefingReply is the part of get_flight_briefing_data's data the tests read.
type briefingReply struct {
	Request struct {
		Airports        []string `json:"airports"`
		Centers         []string `json:"centers"`
		Include         []string `json:"include"`
		IncludeGeometry bool     `json:"include_geometry"`
	} `json:"request"`
	Translation []translationEntry `json:"translation"`
	Locations   *struct {
		Requested []string `json:"requested"`
		Added     []struct {
			Location string `json:"location"`
		} `json:"added"`
		Note string `json:"note"`
	} `json:"locations"`
	OmittedFields []string `json:"omitted_fields"`
	raw           map[string]json.RawMessage
}

func decodeBriefing(t *testing.T, r reply) briefingReply {
	t.Helper()
	var b briefingReply
	if err := json.Unmarshal(r.Data, &b); err != nil {
		t.Fatalf("data: %v\n%s", err, r.Data)
	}
	if err := json.Unmarshal(r.Data, &b.raw); err != nil {
		t.Fatal(err)
	}
	return b
}

// items decodes one sublist; nil when the key is absent.
func (b briefingReply) items(t *testing.T, key string) []map[string]json.RawMessage {
	t.Helper()
	raw, ok := b.raw[key]
	if !ok {
		return nil
	}
	out := []map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("%s: %v", key, err)
	}
	return out
}

func sourceIDs(r reply) map[string]bool {
	ids := map[string]bool{}
	for _, s := range r.Provenance.Sources {
		ids[s.ID] = true
	}
	return ids
}

func TestTranslateIsShownPerSource(t *testing.T) {
	got := translate(briefingRequest{
		Airports: []string{"KSMO", "KSNA"},
		Centers:  []string{"ZAB"},
		Include:  []string{"notams", "metars", "tafs", "sigmets", "airmets", "intl_sigmets", "cwas"},
	})
	place := []string{"airports", "centers"}
	want := []translationEntry{
		{Source: "faa.nms.notam", AppliedBySource: map[string]string{"location": "KSMO,KSNA,ZAB"}, AppliedByServer: map[string]string{}, NotUsed: []string{}},
		{Source: "awc.metar", AppliedBySource: map[string]string{"ids": "KSMO,KSNA"}, AppliedByServer: map[string]string{}, NotUsed: []string{"centers"}},
		{Source: "awc.taf", AppliedBySource: map[string]string{"ids": "KSMO,KSNA"}, AppliedByServer: map[string]string{}, NotUsed: []string{"centers"}},
		{Source: "awc.airsigmet", AppliedBySource: map[string]string{}, AppliedByServer: map[string]string{}, NotUsed: place},
		{Source: "awc.gairmet", AppliedBySource: map[string]string{}, AppliedByServer: map[string]string{}, NotUsed: place},
		{Source: "awc.isigmet", AppliedBySource: map[string]string{}, AppliedByServer: map[string]string{}, NotUsed: place},
		{Source: "awc.cwa", AppliedBySource: map[string]string{}, AppliedByServer: map[string]string{}, NotUsed: place},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("translate =\n%+v\nwant\n%+v", got, want)
	}

	// Without centers, METAR and TAF use all of the input.
	got = translate(briefingRequest{Airports: []string{"KSMO"}, Include: []string{"metars"}})
	if len(got) != 1 || got[0].Source != "awc.metar" || len(got[0].NotUsed) != 0 || got[0].NotUsed == nil {
		t.Errorf("translate without centers = %+v", got)
	}
}

func TestBriefingReturnsEverySublistWithOneEnvelope(t *testing.T) {
	u := newUpstreams(t)
	cs := connect(t, u.deps(t, true, false))
	res, r := call(t, cs, "get_flight_briefing_data", map[string]any{"airports": []string{"KSMO", "KSNA"}})
	if res.IsError {
		t.Fatalf("error: %s", text(res))
	}
	b := decodeBriefing(t, r)
	for _, k := range briefingSublists {
		if _, ok := b.raw[k]; !ok {
			t.Errorf("data lacks %q", k)
		}
	}
	if strings.Join(b.OmittedFields, ",") != "geometry" {
		t.Errorf("omitted_fields = %v", b.OmittedFields)
	}
	for _, k := range []string{"notams", "sigmets", "airmets", "intl_sigmets", "cwas"} {
		for _, it := range b.items(t, k) {
			if _, ok := it["geometry"]; ok {
				t.Fatalf("%s item has geometry though include_geometry is false", k)
			}
			if _, ok := it["geometry_derivation"]; ok {
				t.Fatalf("%s item has geometry_derivation though include_geometry is false", k)
			}
		}
	}
	if len(b.items(t, "metars")) == 0 || len(b.items(t, "tafs")) == 0 || len(b.items(t, "notams")) == 0 || len(b.items(t, "airmets")) == 0 {
		t.Errorf("expected records in every fed sublist: %s", r.Data)
	}
	ids := sourceIDs(r)
	for _, id := range []string{"faa.nms.notam:KSMO", "faa.nms.notam:KSNA", "faa.nms.notam:ZDC", "awc.metar", "awc.taf",
		"awc.airsigmet", "awc.gairmet", "awc.isigmet", "awc.cwa"} {
		if !ids[id] {
			t.Errorf("provenance lacks %s (have %v)", id, ids)
		}
	}
	if b.Locations == nil || b.Locations.Note != centersNotInferred || strings.Join(b.Locations.Requested, ",") != "KSMO,KSNA" {
		t.Errorf("locations = %+v", b.Locations)
	}
	if len(b.Translation) != 7 {
		t.Errorf("translation = %+v", b.Translation)
	}
	// Every entry says who applied each filter: the server applies none, so
	// applied_by_server is present and empty, never null or absent.
	var entries []map[string]json.RawMessage
	if err := json.Unmarshal(b.raw["translation"], &entries); err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if string(e["applied_by_server"]) != "{}" {
			t.Errorf("translation entry %s: applied_by_server = %s, want {}", e["source"], e["applied_by_server"])
		}
	}
	if !strings.Contains(u.awcQueries["/metar"], "ids=KSMO,KSNA") || !strings.Contains(u.awcQueries["/taf"], "ids=KSMO,KSNA") {
		t.Errorf("AWC queries = %v", u.awcQueries)
	}
	// Records returned per source match the response.
	for _, s := range r.Provenance.Sources {
		if s.ID == "awc.gairmet" && s.RecordsReturned != len(b.items(t, "airmets")) {
			t.Errorf("awc.gairmet records_returned = %d, airmets = %d", s.RecordsReturned, len(b.items(t, "airmets")))
		}
		if s.ID == "awc.metar" && s.RecordsReturned != len(b.items(t, "metars")) {
			t.Errorf("awc.metar records_returned = %d, metars = %d", s.RecordsReturned, len(b.items(t, "metars")))
		}
	}
}

func TestBriefingIncludeSelectsSublists(t *testing.T) {
	u := newUpstreams(t)
	cs := connect(t, u.deps(t, true, false))
	res, r := call(t, cs, "get_flight_briefing_data", map[string]any{"airports": []string{"KSMO"}, "include": []string{"tafs", "sigmets"}})
	if res.IsError {
		t.Fatal(text(res))
	}
	b := decodeBriefing(t, r)
	for _, k := range briefingSublists {
		_, ok := b.raw[k]
		if want := k == "tafs" || k == "sigmets"; ok != want {
			t.Errorf("key %s present = %v, want %v", k, ok, want)
		}
	}
	if _, ok := b.raw["locations"]; ok {
		t.Error("locations is only for NOTAMs")
	}
	ids := sourceIDs(r)
	if len(ids) != 2 || !ids["awc.taf"] || !ids["awc.airsigmet"] {
		t.Errorf("sources = %v", ids)
	}
	if len(u.notamQueries) != 0 {
		t.Errorf("NMS was queried: %v", u.notamQueries)
	}
	if strings.Join(b.Request.Include, ",") != "tafs,sigmets" {
		t.Errorf("request.include = %v", b.Request.Include)
	}
	if len(b.Translation) != 2 || b.Translation[0].Source != "awc.taf" || b.Translation[1].Source != "awc.airsigmet" {
		t.Errorf("translation = %+v", b.Translation)
	}
}

func TestBriefingGeometryOnRequest(t *testing.T) {
	cs := connect(t, newUpstreams(t).deps(t, true, false))
	res, r := call(t, cs, "get_flight_briefing_data", map[string]any{"airports": []string{"KSMO"}, "include_geometry": true})
	if res.IsError {
		t.Fatal(text(res))
	}
	b := decodeBriefing(t, r)
	if b.OmittedFields == nil || len(b.OmittedFields) != 0 {
		t.Errorf("omitted_fields = %v, want []", b.OmittedFields)
	}
	if !b.Request.IncludeGeometry {
		t.Error("request.include_geometry should be true")
	}
	with := 0
	for _, it := range b.items(t, "airmets") {
		if _, ok := it["geometry"]; ok {
			with++
		}
	}
	if with == 0 {
		t.Error("advisories carry no geometry though it was requested")
	}
}

func TestBriefingNormalizesAirports(t *testing.T) {
	u := newUpstreams(t)
	cs := connect(t, u.deps(t, true, false))
	res, r := call(t, cs, "get_flight_briefing_data", map[string]any{"airports": []string{"ksmo", " KSMO", "KSNA"}})
	if res.IsError {
		t.Fatal(text(res))
	}
	b := decodeBriefing(t, r)
	if strings.Join(b.Request.Airports, ",") != "KSMO,KSNA" {
		t.Errorf("request.airports = %v", b.Request.Airports)
	}
	n := 0
	for _, l := range u.queriedLocations() {
		if l == "KSMO" || l == "KSNA" {
			n++
		} else if l != "ZDC" {
			t.Errorf("unexpected FAA query for %q", l)
		}
	}
	if n != 2 {
		t.Errorf("FAA airport queries = %d, want 2 (%v)", n, u.queriedLocations())
	}
	if !strings.Contains(u.awcQueries["/metar"], "ids=KSMO,KSNA&") {
		t.Errorf("metar query = %q", u.awcQueries["/metar"])
	}
}

func TestBriefingCenterInAirportsIsFlaggedNotHidden(t *testing.T) {
	u := newUpstreams(t)
	cs := connect(t, u.deps(t, true, false))
	res, r := call(t, cs, "get_flight_briefing_data", map[string]any{"airports": []string{"KZLA"}})
	if res.IsError {
		t.Fatal(text(res))
	}
	found := false
	for _, l := range u.queriedLocations() {
		found = found || l == "KZLA"
	}
	if !found {
		t.Errorf("no FAA query for KZLA: %v", u.queriedLocations())
	}
	for _, src := range []string{"awc.metar", "awc.taf"} {
		ok := false
		for _, i := range r.Completeness.Issues {
			if i.Code == "STATION_NO_DATA" && i.Source == src {
				for _, ref := range i.Refs {
					ok = ok || ref == "KZLA"
				}
			}
		}
		if !ok {
			t.Errorf("no STATION_NO_DATA for KZLA from %s: %+v", src, r.Completeness.Issues)
		}
	}
}

func TestBriefingCentersGoToNOTAMsOnly(t *testing.T) {
	u := newUpstreams(t)
	cs := connect(t, u.deps(t, true, false))
	res, r := call(t, cs, "get_flight_briefing_data", map[string]any{"airports": []string{"KSMO"}, "centers": []string{"kzab"}})
	if res.IsError {
		t.Fatal(text(res))
	}
	found := false
	for _, l := range u.queriedLocations() {
		found = found || l == "ZAB"
	}
	if !found {
		t.Errorf("no FAA query for ZAB: %v", u.queriedLocations())
	}
	if q := u.awcQueries["/metar"]; strings.Contains(q, "ZAB") || !strings.Contains(q, "ids=KSMO&") {
		t.Errorf("metar query = %q", q)
	}
	b := decodeBriefing(t, r)
	if strings.Join(b.Request.Centers, ",") != "ZAB" {
		t.Errorf("request.centers = %v", b.Request.Centers)
	}
}

func TestBriefingWithoutNOTAMSourceStillReturnsWeather(t *testing.T) {
	cs := connect(t, newUpstreams(t).deps(t, false, false))
	res, r := call(t, cs, "get_flight_briefing_data", map[string]any{"airports": []string{"KSMO"}})
	if res.IsError {
		t.Fatalf("weather was obtained, so this is not an error: %s", text(res))
	}
	if r.Completeness.Status != "incomplete" || !r.hasIssue("SOURCE_NOT_ENABLED", "") {
		t.Errorf("status = %s issues = %+v", r.Completeness.Status, r.Completeness.Issues)
	}
	b := decodeBriefing(t, r)
	if len(b.items(t, "metars")) == 0 {
		t.Error("metars must still be returned")
	}
	if n := b.items(t, "notams"); n == nil || len(n) != 0 {
		t.Errorf("notams = %v, want []", n)
	}
	if b.Locations == nil || strings.Join(b.Locations.Requested, ",") != "KSMO" || b.Locations.Note != centersNotInferred {
		t.Errorf("locations = %+v", b.Locations)
	}
}

func TestBriefingOneFailedSourceIsVisible(t *testing.T) {
	u := newUpstreams(t)
	u.awcFail["/taf"] = 503
	cs := connect(t, u.deps(t, true, false))
	res, r := call(t, cs, "get_flight_briefing_data", map[string]any{"airports": []string{"KSMO"}})
	if res.IsError {
		t.Fatal("one failed source must not make the call an error")
	}
	if r.Completeness.Status != "incomplete" || !r.hasIssue("SOURCE_UNAVAILABLE", "awc.taf") {
		t.Errorf("status = %s issues = %+v", r.Completeness.Status, r.Completeness.Issues)
	}
	b := decodeBriefing(t, r)
	if len(b.items(t, "metars")) == 0 {
		t.Error("metars must still be returned")
	}
	if tafs := b.items(t, "tafs"); tafs == nil || len(tafs) != 0 {
		t.Errorf("tafs = %v, want []", tafs)
	}
	if !sourceIDs(r)["awc.taf"] {
		t.Error("the failed source must still be listed")
	}
}

func TestBriefingAllSourcesFailedIsAnError(t *testing.T) {
	u := newUpstreams(t)
	u.nmsFail = 503
	for _, p := range []string{"/metar", "/taf", "/airsigmet", "/gairmet", "/isigmet", "/cwa"} {
		u.awcFail[p] = 503
	}
	cs := connect(t, u.deps(t, true, false))
	res, r := call(t, cs, "get_flight_briefing_data", map[string]any{"airports": []string{"KSMO"}})
	if !res.IsError {
		t.Fatal("every source failed: isError must be true")
	}
	if res.StructuredContent == nil || r.Completeness.Status != "incomplete" || len(r.Provenance.Sources) == 0 {
		t.Errorf("the envelope must still be present: %s", text(res))
	}
}

func TestBriefingInputErrors(t *testing.T) {
	eleven := []string{"KAAA", "KBBB", "KCCC", "KDDD", "KEEE", "KFFF", "KGGG", "KHHH", "KIII", "KJJJ", "KKKK"}
	cases := []struct {
		name string
		args map[string]any
		want string
	}{
		{"3-letter", map[string]any{"airports": []string{"SMO"}}, "airports: SMO is not a 4-character ICAO identifier; use the ICAO form, e.g. KSMO"},
		{"selector", map[string]any{"airports": []string{"@CA"}}, "airports: @CA is not a 4-character ICAO identifier"},
		{"empty", map[string]any{"airports": []string{}}, "airports is required"},
		{"missing", map[string]any{}, `"airports"`}, // the input schema marks it required
		{"eleven airports", map[string]any{"airports": eleven}, "at most 10 airports and centers per call (got 11); split the request"},
		{"eight plus three", map[string]any{"airports": eleven[:8], "centers": []string{"ZLA", "ZAB", "ZOA"}}, "at most 10 airports and centers per call (got 11); split the request"},
		{"bad center", map[string]any{"airports": []string{"KSMO"}, "centers": []string{"LAX"}}, "centers: LAX is not an en-route center identifier such as ZLA"},
		{"empty include", map[string]any{"airports": []string{"KSMO"}, "include": []string{}}, "include must not be empty; omit it for everything"},
		{"bad include", map[string]any{"airports": []string{"KSMO"}, "include": []string{"pireps"}}, "include: pireps is not one of notams, metars, tafs, sigmets, airmets, intl_sigmets, cwas"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			u := newUpstreams(t)
			cs := connect(t, u.deps(t, true, false))
			res, _ := call(t, cs, "get_flight_briefing_data", c.args)
			if !res.IsError || !strings.Contains(text(res), c.want) {
				t.Errorf("isError = %v text = %q, want %q", res.IsError, text(res), c.want)
			}
			if len(u.awcCalls) != 0 || len(u.notamQueries) != 0 {
				t.Errorf("an input error must make no upstream request: awc %v nms %v", u.awcCalls, u.notamQueries)
			}
		})
	}
}

func TestBriefingNOTAMsOnlyWithSourceOffIsAnError(t *testing.T) {
	cs := connect(t, newUpstreams(t).deps(t, false, false))
	res, r := call(t, cs, "get_flight_briefing_data", map[string]any{"airports": []string{"KSMO"}, "include": []string{"notams"}})
	if !res.IsError {
		t.Fatal("the only included source is not enabled: isError must be true")
	}
	if res.StructuredContent == nil || r.Completeness.Status != "incomplete" || !r.hasIssue("SOURCE_NOT_ENABLED", "faa.nms.notam") {
		t.Errorf("the envelope must still be present and say why: %s", text(res))
	}
	if b := decodeBriefing(t, r); b.items(t, "notams") == nil {
		t.Error("notams must be present as [] so the gap is visible")
	}
}

func TestBriefingEveryIncludedSourceFailedIsAnError(t *testing.T) {
	u := newUpstreams(t)
	u.awcFail["/metar"] = 503
	cs := connect(t, u.deps(t, false, false))
	res, r := call(t, cs, "get_flight_briefing_data", map[string]any{"airports": []string{"KSMO"}, "include": []string{"notams", "metars"}})
	if !res.IsError {
		t.Fatal("NMS is off and METAR failed: isError must be true")
	}
	if res.StructuredContent == nil || !r.hasIssue("SOURCE_NOT_ENABLED", "faa.nms.notam") || !r.hasIssue("SOURCE_UNAVAILABLE", "awc.metar") {
		t.Errorf("the envelope must still be present: %s", text(res))
	}
}
