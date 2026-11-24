package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/davidhinkes/hobbs/internal/model"
	"github.com/davidhinkes/hobbs/internal/provenance"
	"github.com/davidhinkes/hobbs/internal/source"
	"github.com/davidhinkes/hobbs/internal/source/nms"
)

// NotamsInput is the input of get_notams. Every filter is forwarded to the
// FAA NMS-API, which applies it; the server adds none of its own.
type NotamsInput struct {
	Locations       []string `json:"locations,omitempty" jsonschema:"Location identifiers, up to 10: airports (ICAO or domestic, e.g. KSMO or SMO) and en-route centers (ARTCC, e.g. ZLA). The FAA is asked once per identifier, with any other filters applied to each, and the results are merged: a NOTAM returned for several identifiers appears once, with returned_for listing them. Identifiers are the FAA's associations: a navaid identifier may return the NOTAMs of an airport the FAA ties it to."`
	AddCenters      *bool    `json:"add_centers,omitempty" jsonschema:"Optional, default true, used with locations. Also asks for each en-route center (ARTCC) that the FAA lists on the NOTAMs returned for the given locations, if it is not already in the list; the response lists each added center under locations.added and why. Centers between the given locations are never inferred: a flight that passes through other centers must list them."`
	Latitude        *float64 `json:"latitude,omitempty" jsonschema:"Decimal degrees, -90 to 90. Must be given together with longitude and radius_nm."`
	Longitude       *float64 `json:"longitude,omitempty" jsonschema:"Decimal degrees, -180 to 180. Must be given together with latitude and radius_nm."`
	RadiusNM        *float64 `json:"radius_nm,omitempty" jsonschema:"Search radius in nautical miles, 0 to 100 (the API's limit). Must be given together with latitude and longitude. The FAA matches each NOTAM's reference point, not its area: a NOTAM whose area reaches into the circle but whose reference point is farther away is not returned."`
	EffectiveStart  string   `json:"effective_start,omitempty" jsonschema:"RFC 3339 UTC timestamp. Must be given together with effective_end. The API returns NOTAMs effective in the window."`
	EffectiveEnd    string   `json:"effective_end,omitempty" jsonschema:"RFC 3339 UTC timestamp. Must be given together with effective_start."`
	LastUpdated     string   `json:"last_updated,omitempty" jsonschema:"RFC 3339 UTC timestamp within the previous 24 hours; returns NOTAMs created, updated or cancelled since then, active or not."`
	Classification  string   `json:"classification,omitempty" jsonschema:"One of DOMESTIC, INTERNATIONAL, MILITARY, LOCAL_MILITARY, FDC. Needs another filter alongside it. Note records report the short form (DOM, INTL, MIL, FDC)."`
	Feature         string   `json:"feature,omitempty" jsonschema:"NOTAM feature: RWY, TWY, APRON, AD, OBST, NAV, COM, SVC, AIRSPACE, ODP, SID, STAR, CHART, DATA, DVA, IAP, VFP, ROUTE, SPECIAL or SECURITY."`
	Accountability  string   `json:"accountability,omitempty" jsonschema:"Accountability code, e.g. FDC or an airport id."`
	FreeText        string   `json:"free_text,omitempty" jsonschema:"Free-text search, evaluated by the API."`
	NotamNumber     string   `json:"notam_number,omitempty" jsonschema:"NOTAM number, e.g. 08/224 or A4429/26. Needs locations or accountability."`
	Limit           *int     `json:"limit,omitempty" jsonschema:"Optional. Return at most this many items. There is no default cap; if you set one and items are omitted, the response says how many (RESULT_TRUNCATED) and is not complete."`
	IncludeGeometry *bool    `json:"include_geometry,omitempty" jsonschema:"Optional, default true. Set false to leave geometry out of each item to make the response much smaller; the response lists geometry under omitted_fields."`
}

// maxLocations bounds how many identifiers one call may ask for.
const maxLocations = 10

// notEnabled is the one name for "the NOTAM source is turned off", used in
// not_consulted and in source_status.
const (
	notEnabled     = "not_enabled"
	notEnabledText = "NOTAMs are unavailable: the NOTAM source is not enabled on this server"
)

// centersNotInferred is stated in every response that used locations.
const centersNotInferred = "Only en-route centers that the FAA lists on the NOTAMs of the given locations are added. " +
	"Centers between those locations are not inferred: if the flight passes through others, list them."

type notamData struct {
	Filters       Filters        `json:"filters"`
	Locations     *locationsInfo `json:"locations,omitempty"`
	OmittedFields []string       `json:"omitted_fields"`
	NOTAMs        []model.Notam  `json:"notams"`
}

// locationsInfo says which identifiers were asked for and why.
type locationsInfo struct {
	Requested []string      `json:"requested"`
	Added     []addedCenter `json:"added"`
	Note      string        `json:"note"`
}

// addedCenter is an en-route center added because the FAA lists it on the
// NOTAMs of a requested location.
type addedCenter struct {
	Location string   `json:"location"`
	ListedOn []string `json:"listed_on"` // requested locations whose NOTAMs name it
	Refs     []string `json:"refs"`      // some of those NOTAMs
}

func registerNotams(srv *mcp.Server, d Deps) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "get_notams",
		Annotations: readOnly("NOTAMs from the FAA NOTAM Management Service"),
		Description: d.describe("Returns NOTAMs from the FAA NOTAM Management Service (NMS-API), including FDC NOTAMs. " +
			"You must give at least one filter: usually locations (airports and en-route centers, e.g. departure, destination and alternates), or a latitude/longitude/radius_nm point, an effective window, or others. " +
			"The filters are applied by the FAA's API, not by this server, and the response lists exactly which. " +
			"For a flight, list the airports; the en-route center the FAA lists on their NOTAMs is added unless add_centers is false, and is named in the response. Centers between them are not inferred. " +
			"A radius search matches each NOTAM's reference point, not its area, so it is not an area filter. " +
			"Each NOTAM carries its full text as published in raw_text: the ICAO rendering (raw_format icao_notam) when the FAA supplied one, else the US domestic one (us_notam, which the FAA calls LOCAL_FORMAT); any other rendering is in translations, so each appears once; the published fields, and canonical GeoJSON geometry where one is published (positions are [longitude, latitude]; a point with a radius is translated to a slightly larger circle polygon, with the original circle in geometry_derivation). " +
			"Results can be large (an airport can have hundreds of NOTAMs); nothing is ranked, summarized or dropped. The same TFR or restriction can appear twice, as an FDC NOTAM and in its international (ICAO) form; both are returned. " +
			"NOTAMs for temporary flight restrictions appear here as FDC NOTAMs with their text and dates, but the FAA's TFR outlines are not available from this source, so geometry for them may be missing or only a point. " + reliance),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in NotamsInput) (*mcp.CallToolResult, any, error) {
		return notamsHandler(ctx, d, in)
	})
}

// fetch is one query to the FAA and its outcome.
type fetch struct {
	location string // "" when the call used no locations
	snap     source.Snapshot[model.Notam]
	err      error
}

// sourceID names one query in provenance: the NOTAM source, qualified by the
// location asked for.
func (f fetch) sourceID() string {
	if f.location == "" {
		return nms.SourceID
	}
	return nms.SourceID + ":" + f.location
}

func notamsHandler(ctx context.Context, d Deps, in NotamsInput) (*mcp.CallToolResult, any, error) {
	base := nms.Query{
		Latitude: in.Latitude, Longitude: in.Longitude, RadiusNM: in.RadiusNM,
		Classification: strings.TrimSpace(in.Classification), Feature: strings.TrimSpace(in.Feature),
		Accountability: strings.TrimSpace(in.Accountability), FreeText: strings.TrimSpace(in.FreeText), NotamNumber: strings.TrimSpace(in.NotamNumber),
	}
	if err := checkLimit(in.Limit); err != nil {
		return toolError("%v", err)
	}
	var err error
	if base.EffectiveStart, err = parseTime("effective_start", in.EffectiveStart); err != nil {
		return toolError("%v", err)
	}
	if base.EffectiveEnd, err = parseTime("effective_end", in.EffectiveEnd); err != nil {
		return toolError("%v", err)
	}
	if base.LastUpdated, err = parseTime("last_updated", in.LastUpdated); err != nil {
		return toolError("%v", err)
	}
	locs, err := cleanLocations(in.Locations)
	if err != nil {
		return toolError("%v", err)
	}
	if len(locs) == 0 {
		locs = []string{""} // one query, with no location
	}
	for _, l := range locs {
		q := base
		q.Location = l
		if err := q.Validate(); err != nil {
			return toolError("%v", err) // the request itself is wrong; the agent can fix it
		}
	}

	now := d.Now().UTC()
	data := notamData{Filters: newFilters(), NOTAMs: []model.Notam{}, OmittedFields: []string{}}
	if locs[0] != "" {
		data.Locations = &locationsInfo{Requested: locs, Added: []addedCenter{}, Note: centersNotInferred}
	}

	if d.NMS == nil {
		// The NOTAM source is not enabled: say so rather than return an empty list.
		return respond(d, data, now, nil, []provenance.NotConsulted{{ID: nms.SourceID, Reason: notEnabled}}, []provenance.Issue{{
			Code: provenance.SourceNotEnabled, Severity: provenance.Critical, Source: nms.SourceID,
			Message: notEnabledText,
		}}, true)
	}

	fetches := run(ctx, d.NMS, base, locs)
	var issues []provenance.Issue
	if data.Locations != nil && (in.AddCenters == nil || *in.AddCenters) {
		added, missing := impliedCenters(fetches, locs)
		data.Locations.Added = added
		issues = append(issues, missing...)
		extra := make([]string, len(added))
		for i, a := range added {
			extra[i] = a.Location
		}
		fetches = append(fetches, run(ctx, d.NMS, base, extra)...)
	}

	// Merge in query order; a NOTAM returned by several queries appears once.
	index := map[string]int{}
	var sources []provenance.Source
	failed := 0
	for _, f := range fetches {
		if f.err != nil {
			if errors.Is(f.err, nms.ErrBadQuery) {
				return toolError("%v", f.err)
			}
			s := f.snap.Source
			s.ID = f.sourceID()
			s, is := failure(s, s.ID, f.err, now)
			sources, issues = append(sources, s), append(issues, is)
			failed++
			continue
		}
		for _, is := range f.snap.Issues {
			is.Source = f.sourceID()
			issues = append(issues, is)
		}
		for _, n := range f.snap.Items {
			k := notamKey(n)
			i, seen := index[k]
			if !seen {
				i = len(data.NOTAMs)
				index[k] = i
				data.NOTAMs = append(data.NOTAMs, n)
			}
			if f.location != "" {
				data.NOTAMs[i].ReturnedFor = append(data.NOTAMs[i].ReturnedFor, f.location)
			}
		}
	}

	data.Filters.AppliedBySource = base.Applied()
	if data.Locations != nil {
		all := locs
		for _, a := range data.Locations.Added {
			all = append(all, a.Location)
		}
		data.Filters.AppliedBySource["location"] = strings.Join(all, ",")
	}

	// The caller's explicit shaping choices.
	if in.Limit != nil && len(data.NOTAMs) > *in.Limit {
		issues = append(issues, truncationIssue(len(data.NOTAMs), *in.Limit))
		data.NOTAMs = data.NOTAMs[:*in.Limit]
	}
	if in.IncludeGeometry != nil && !*in.IncludeGeometry {
		for i := range data.NOTAMs {
			data.NOTAMs[i].Geometry, data.NOTAMs[i].GeometryDerivation = nil, nil
		}
		data.OmittedFields = append(data.OmittedFields, "geometry")
	}

	// Each successful query's source, with how many of its records made it
	// into the response.
	returned := map[string]int{}
	for _, n := range data.NOTAMs {
		for _, l := range n.ReturnedFor {
			returned[l]++
		}
	}
	for _, f := range fetches {
		if f.err != nil {
			continue
		}
		s := f.snap.Source
		s.ID = f.sourceID()
		s.RecordsReturned = returned[f.location]
		if f.location == "" {
			s.RecordsReturned = len(data.NOTAMs)
		}
		sources = append(sources, s)
	}
	return respond(d, data, now, sources, nil, issues, failed == len(fetches))
}

// run queries the FAA once per location, concurrently, keeping the order.
func run(ctx context.Context, c *nms.Client, base nms.Query, locs []string) []fetch {
	out := make([]fetch, len(locs))
	parallel(len(locs), func(i int) {
		q := base
		q.Location = locs[i]
		snap, err := c.Query(ctx, q)
		out[i] = fetch{location: locs[i], snap: snap, err: err}
	})
	return out
}

// cleanLocations trims, drops exact duplicates (ignoring case) and bounds the
// list. It does not rewrite identifiers: the FAA decides what each one means.
func cleanLocations(in []string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, l := range in {
		l = strings.TrimSpace(l)
		if l == "" {
			return nil, errors.New("locations must not contain an empty identifier")
		}
		if seen[strings.ToUpper(l)] {
			continue
		}
		seen[strings.ToUpper(l)] = true
		out = append(out, l)
	}
	if len(out) > maxLocations {
		return nil, fmt.Errorf("at most %d locations per call (got %d); split the request", maxLocations, len(out))
	}
	return out, nil
}

// centerKey makes the two spellings of a US en-route center the FAA uses
// (ZLA and KZLA) compare equal. Every other identifier is left as is.
func centerKey(s string) string {
	s = strings.ToUpper(strings.TrimSpace(s))
	if len(s) == 4 && strings.HasPrefix(s, "KZ") {
		return s[1:]
	}
	return s
}

// impliedCenters returns the en-route centers the FAA lists (affectedFir) on
// the NOTAMs of the requested locations that are not already requested, and a
// warning for each requested location whose NOTAMs list none. It only ever
// adds queries; it never removes or narrows anything.
func impliedCenters(fetches []fetch, requested []string) ([]addedCenter, []provenance.Issue) {
	have := map[string]bool{}
	for _, l := range requested {
		have[centerKey(l)] = true
	}
	found := map[string]*addedCenter{}
	var issues []provenance.Issue
	for _, f := range fetches {
		if f.err != nil {
			continue // already reported as unavailable
		}
		listed := false
		for _, n := range f.snap.Items {
			if n.FIR == "" {
				continue
			}
			listed = true
			k := centerKey(n.FIR)
			if have[k] {
				continue
			}
			a := found[k]
			if a == nil {
				a = &addedCenter{Location: k}
				found[k] = a
			}
			if len(a.ListedOn) == 0 || a.ListedOn[len(a.ListedOn)-1] != f.location {
				a.ListedOn = append(a.ListedOn, f.location)
			}
			if len(a.Refs) < source.MaxRefs {
				a.Refs = append(a.Refs, n.ID)
			}
		}
		if !listed {
			issues = append(issues, provenance.Issue{
				Code: provenance.CenterNotImplied, Severity: provenance.Warning, Source: f.sourceID(),
				Message: fmt.Sprintf("no NOTAM returned for %s lists an en-route center, so none was added for it; if the flight needs its center, list it", f.location),
			})
		}
	}
	out := make([]addedCenter, 0, len(found))
	for _, a := range found {
		out = append(out, *a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Location < out[j].Location })
	return out, issues
}

// notamKey identifies a NOTAM across queries: the source's unique id when it
// has one, else the number, location, text and source record together (a
// stub's id is only its position in one query, so it must not be the key).
func notamKey(n model.Notam) string {
	if n.NMSID != "" {
		return n.NMSID
	}
	return n.ID + "|" + n.Location + "|" + n.RawText + "|" + string(n.SourceRecord)
}
