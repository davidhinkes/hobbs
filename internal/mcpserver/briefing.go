package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/davidhinkes/hobbs/internal/model"
	"github.com/davidhinkes/hobbs/internal/provenance"
	"github.com/davidhinkes/hobbs/internal/source"
	"github.com/davidhinkes/hobbs/internal/source/awc"
	"github.com/davidhinkes/hobbs/internal/source/nms"
)

// BriefingInput is the input of get_flight_briefing_data. It takes places
// only (airports and centers), never a time window, a limit or a filter of
// this server's own: each source is asked with its own filters, as shown in
// the response's translation.
type BriefingInput struct {
	Airports        []string `json:"airports" jsonschema:"Required. The flight's airports (departure, destination, alternates), 1 to 10, each a 4-character ICAO identifier such as KSMO (case-insensitive; duplicates are removed). A 3-letter FAA identifier such as SMO is rejected: use the ICAO form, the server never adds a K. Selectors such as @CA are rejected. Airports plus centers are at most 10."`
	Centers         []string `json:"centers,omitempty" jsonschema:"Optional. En-route centers (ARTCC) the flight also crosses, e.g. ZAB (KZAB is the same). Used for NOTAMs only, not for METARs or TAFs. The center the FAA lists on the airports' NOTAMs is added anyway; centers between the airports are never inferred, so list any other center the flight crosses. Airports plus centers are at most 10."`
	Include         []string `json:"include,omitempty" jsonschema:"Optional. Which sublists to return, from: notams, metars, tafs, sigmets, airmets, intl_sigmets, cwas. Default: all seven. Must not be empty when given. Output is always in that order."`
	IncludeGeometry *bool    `json:"include_geometry,omitempty" jsonschema:"Optional, default false. Set true to include each record's geometry; without it the response is much smaller and lists geometry under omitted_fields."`
}

// briefingRequest is the input as understood: upper-cased, de-duplicated,
// centers in their short form (ZLA), include in output order. It is returned
// in the response so the caller can see what was asked for.
type briefingRequest struct {
	Airports        []string `json:"airports"`
	Centers         []string `json:"centers"`
	Include         []string `json:"include"`
	IncludeGeometry bool     `json:"include_geometry"`
}

// translationEntry is what one source was asked: the filters sent, which the
// source applied (applied_by_source) and which the server applied
// (applied_by_server: always {}, because this tool applies no filter of its
// own), and which parts of the input it did not use.
type translationEntry struct {
	Source          string            `json:"source"`
	AppliedBySource map[string]string `json:"applied_by_source"`
	AppliedByServer map[string]string `json:"applied_by_server"`
	NotUsed         []string          `json:"not_used"`
}

// briefingData is the data of a get_flight_briefing_data response. A sublist
// that was not included is absent; an included one is a list, [] when empty.
type briefingData struct {
	Request       briefingRequest        `json:"request"`
	Translation   []translationEntry     `json:"translation"`
	Locations     *locationsInfo         `json:"locations,omitempty"`
	OmittedFields []string               `json:"omitted_fields"`
	NOTAMs        *[]model.Notam         `json:"notams,omitempty"`
	METARs        *[]model.StationReport `json:"metars,omitempty"`
	TAFs          *[]model.StationReport `json:"tafs,omitempty"`
	SIGMETs       *[]model.Advisory      `json:"sigmets,omitempty"`
	AIRMETs       *[]model.Advisory      `json:"airmets,omitempty"`
	IntlSIGMETs   *[]model.Advisory      `json:"intl_sigmets,omitempty"`
	CWAs          *[]model.Advisory      `json:"cwas,omitempty"`
}

// briefingIncludes are the include values, in output order.
var briefingIncludes = []string{"notams", "metars", "tafs", "sigmets", "airmets", "intl_sigmets", "cwas"}

// advisoryInclude ties an advisory include value to its weatherFeeds kind and
// provenance source id.
var advisoryInclude = map[string]struct{ kind, id string }{
	"sigmets":      {"SIGMET", awc.IDAirSigmet},
	"airmets":      {"AIRMET", awc.IDGAirmet},
	"intl_sigmets": {"INTL_SIGMET", awc.IDIntlSigmet},
	"cwas":         {"CWA", awc.IDCWA},
}

var (
	icaoID   = regexp.MustCompile(`^[A-Z0-9]{4}$`)
	centerID = regexp.MustCompile(`^K?Z[A-Z]{2}$`)
)

func registerBriefing(srv *mcp.Server, d Deps) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "get_flight_briefing_data",
		Annotations: readOnly("NOTAMs, METARs, TAFs and weather advisories for a flight"),
		Description: d.describe("Returns, in one call, the official data for a flight's airports: NOTAMs from the FAA NOTAM Management Service (NMS-API), METARs (including SPECIs) and TAFs as published by the NOAA Aviation Weather Center (AWC), and the AWC's weather advisories (SIGMET, graphical AIRMET, international SIGMET, CWA), each in its own sublist. " + guidePointer + " " + liveDataRule + " " +
			"Give airports as 4-character ICAO identifiers (KSMO, not SMO; the server never adds a K), and optionally centers: en-route centers (e.g. ZAB) the flight also crosses, used for NOTAMs only; include chooses the sublists (default all). " +
			"NOTAMs are asked for each airport and center; the en-route center the FAA lists on their NOTAMs is added and named under locations.added. Centers between the airports are never inferred: list any other center the flight crosses. " +
			"Each NOTAM's raw_text is its ICAO rendering (raw_format icao_notam) when the FAA supplied one, else the US domestic one (us_notam); any other rendering is in translations. " +
			"NOTAMs for temporary flight restrictions appear as FDC NOTAMs with their text and dates, but the FAA's TFR outlines are not available from this source, so their geometry may be missing or only a point. " +
			"METARs and TAFs are returned as published by AWC (raw_format metar or taf), with AWC's decoded fields in details; fltCat in a report's details is AWC's flight category, not computed by hobbs. A requested airport with no report is flagged STATION_NO_DATA. " +
			"The advisories are the full national sets, not filtered by airport or route. translation lists, per source, the parameters sent for the airports and centers given and which parts of the input it did not use; the implied centers the server adds for NOTAMs are named under locations.added and in provenance, not in translation. " +
			"Geometry is left out unless include_geometry is true; omitted_fields then lists it. Nothing is ranked, summarized or dropped, and nothing is decided about what applies to the flight." + reliance),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in BriefingInput) (*mcp.CallToolResult, any, error) {
		return briefingHandler(ctx, d, in)
	})
}

// validateBriefing applies the input rules and returns the input as
// understood. It never rewrites an airport identifier beyond trimming and
// upper-casing.
func validateBriefing(in BriefingInput) (briefingRequest, error) {
	req := briefingRequest{Airports: []string{}, Centers: []string{}}
	if len(in.Airports) == 0 {
		return req, errors.New("airports is required")
	}
	seen := map[string]bool{}
	for _, a := range in.Airports {
		a = strings.ToUpper(strings.TrimSpace(a))
		if !icaoID.MatchString(a) {
			return req, fmt.Errorf("airports: %s is not a 4-character ICAO identifier; use the ICAO form, e.g. KSMO", a)
		}
		if !seen[a] {
			seen[a] = true
			req.Airports = append(req.Airports, a)
		}
	}
	seen = map[string]bool{}
	for _, c := range in.Centers {
		c = strings.ToUpper(strings.TrimSpace(c))
		if !centerID.MatchString(c) {
			return req, fmt.Errorf("centers: %s is not an en-route center identifier such as ZLA", c)
		}
		if c = centerKey(c); !seen[c] {
			seen[c] = true
			req.Centers = append(req.Centers, c)
		}
	}
	if n := len(req.Airports) + len(req.Centers); n > maxLocations {
		return req, fmt.Errorf("at most %d airports and centers per call (got %d); split the request", maxLocations, n)
	}
	if in.Include == nil {
		req.Include = append([]string{}, briefingIncludes...)
	} else {
		if len(in.Include) == 0 {
			return req, errors.New("include must not be empty; omit it for everything")
		}
		want := map[string]bool{}
		for _, v := range in.Include {
			v = strings.ToLower(strings.TrimSpace(v))
			ok := false
			for _, known := range briefingIncludes {
				ok = ok || v == known
			}
			if !ok {
				return req, fmt.Errorf("include: %s is not one of %s", v, strings.Join(briefingIncludes, ", "))
			}
			want[v] = true
		}
		req.Include = []string{}
		for _, v := range briefingIncludes {
			if want[v] {
				req.Include = append(req.Include, v)
			}
		}
	}
	req.IncludeGeometry = in.IncludeGeometry != nil && *in.IncludeGeometry
	return req, nil
}

// translate says what each included source is asked, in include order. It is
// pure: implied centers, found only once the FAA answers, are reported in
// locations.added, not here.
func translate(req briefingRequest) []translationEntry {
	var out []translationEntry
	for _, inc := range req.Include {
		switch inc {
		case "notams":
			locs := append(append([]string{}, req.Airports...), req.Centers...)
			out = append(out, translationEntry{Source: nms.SourceID, AppliedBySource: map[string]string{"location": strings.Join(locs, ",")}, NotUsed: []string{}})
		case "metars", "tafs":
			id := awc.IDMETAR
			if inc == "tafs" {
				id = awc.IDTAF
			}
			notUsed := []string{}
			if len(req.Centers) > 0 {
				notUsed = append(notUsed, "centers")
			}
			out = append(out, translationEntry{Source: id, AppliedBySource: map[string]string{"ids": strings.Join(req.Airports, ",")}, NotUsed: notUsed})
		default:
			out = append(out, translationEntry{Source: advisoryInclude[inc].id, AppliedBySource: map[string]string{}, NotUsed: []string{"airports", "centers"}})
		}
	}
	for i := range out {
		out[i].AppliedByServer = map[string]string{}
	}
	return out
}

// briefingHandler validates the input, asks every included source in
// parallel and returns one envelope. It is an error (with the envelope) only
// when every source that was, or should have been, asked failed.
func briefingHandler(ctx context.Context, d Deps, in BriefingInput) (*mcp.CallToolResult, any, error) {
	req, err := validateBriefing(in)
	if err != nil {
		return toolError("%v", err)
	}
	want := map[string]bool{}
	var feeds []weatherFeed
	for _, inc := range req.Include {
		want[inc] = true
		if a, ok := advisoryInclude[inc]; ok {
			for _, f := range weatherFeeds {
				if f.kind == a.kind {
					feeds = append(feeds, f)
				}
			}
		}
	}

	var (
		notams           notamResult
		notamErr         error
		metars, tafs     source.Snapshot[model.StationReport]
		metarErr, tafErr error
		adv              advisoryResult
		tasks            []func()
	)
	if want["notams"] {
		locs := append(append([]string{}, req.Airports...), req.Centers...)
		tasks = append(tasks, func() { notams, notamErr = collectNotams(ctx, d, nms.Query{}, locs, true) })
	}
	if want["metars"] {
		tasks = append(tasks, func() { metars, metarErr = d.AWC.METARs(ctx, req.Airports) })
	}
	if want["tafs"] {
		tasks = append(tasks, func() { tafs, tafErr = d.AWC.TAFs(ctx, req.Airports) })
	}
	if len(feeds) > 0 {
		tasks = append(tasks, func() { adv = collectAdvisories(ctx, d, feeds) })
	}
	parallel(len(tasks), func(i int) { tasks[i]() })
	if notamErr != nil {
		return toolError("%v", notamErr) // nms.ErrBadQuery: the request itself is wrong
	}

	now := d.Now().UTC()
	data := briefingData{Request: req, Translation: translate(req), OmittedFields: []string{}}
	var (
		sources         []provenance.Source
		issues          []provenance.Issue
		notConsulted    []provenance.NotConsulted
		queries, failed int
	)

	if want["notams"] {
		data.Locations, data.NOTAMs = notams.Locations, &notams.NOTAMs
		sources, issues = append(sources, notams.Sources...), append(issues, notams.Issues...)
		notConsulted = append(notConsulted, notams.NotConsulted...)
		if d.NMS == nil {
			queries, failed = queries+1, failed+1 // the source that should have been asked
		} else {
			queries, failed = queries+notams.Queries, failed+notams.Failed
		}
	}

	stations := func(id string, snap source.Snapshot[model.StationReport], err error) *[]model.StationReport {
		queries++
		if err != nil {
			s, is := failure(snap.Source, id, err, now)
			sources, issues = append(sources, s), append(issues, is)
			failed++
			return &[]model.StationReport{}
		}
		sources, issues = append(sources, snap.Source), append(issues, snap.Issues...)
		items := snap.Items
		if items == nil {
			items = []model.StationReport{}
		}
		return &items
	}
	if want["metars"] {
		data.METARs = stations(awc.IDMETAR, metars, metarErr)
	}
	if want["tafs"] {
		data.TAFs = stations(awc.IDTAF, tafs, tafErr)
	}

	if len(feeds) > 0 {
		returned := map[string]int{}
		for _, list := range adv.ByKind {
			for _, a := range list {
				returned[a.Source]++
			}
		}
		for _, s := range adv.Sources {
			s.RecordsReturned = returned[s.ID]
			sources = append(sources, s)
		}
		issues = append(issues, adv.Issues...)
		queries, failed = queries+adv.Queries, failed+adv.Failed
		for inc, a := range advisoryInclude {
			if !want[inc] {
				continue
			}
			list := adv.ByKind[a.kind]
			if list == nil {
				list = []model.Advisory{}
			}
			data.setAdvisories(inc, list)
		}
	}

	if !req.IncludeGeometry {
		data.omitGeometry()
	}
	return respond(d, data, now, sources, notConsulted, issues, queries > 0 && failed == queries)
}

// setAdvisories puts one advisory kind's records in its sublist.
func (b *briefingData) setAdvisories(include string, list []model.Advisory) {
	switch include {
	case "sigmets":
		b.SIGMETs = &list
	case "airmets":
		b.AIRMETs = &list
	case "intl_sigmets":
		b.IntlSIGMETs = &list
	case "cwas":
		b.CWAs = &list
	}
}

// omitGeometry removes geometry from every record and says so in
// omitted_fields; nothing else is removed.
func (b *briefingData) omitGeometry() {
	if b.NOTAMs != nil {
		for i := range *b.NOTAMs {
			(*b.NOTAMs)[i].Geometry, (*b.NOTAMs)[i].GeometryDerivation = nil, nil
		}
	}
	for _, list := range []*[]model.StationReport{b.METARs, b.TAFs} {
		if list != nil {
			for i := range *list {
				(*list)[i].Geometry = nil
			}
		}
	}
	for _, list := range []*[]model.Advisory{b.SIGMETs, b.AIRMETs, b.IntlSIGMETs, b.CWAs} {
		if list != nil {
			for i := range *list {
				(*list)[i].Geometry, (*list)[i].GeometryDerivation = nil, nil
			}
		}
	}
	b.OmittedFields = append(b.OmittedFields, "geometry")
}
