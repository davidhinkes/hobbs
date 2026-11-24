package mcpserver

import (
	"context"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/davidhinkes/hobbs/internal/model"
	"github.com/davidhinkes/hobbs/internal/provenance"
	"github.com/davidhinkes/hobbs/internal/source"
	"github.com/davidhinkes/hobbs/internal/source/awc"
)

// WeatherInput is the input of get_weather_advisories.
type WeatherInput struct {
	Kinds           []string `json:"kinds,omitempty" jsonschema:"Which advisory sets to return: SIGMET (US SIGMETs incl. convective), AIRMET (graphical AIRMETs), INTL_SIGMET (international SIGMETs), CWA (center weather advisories). Default: all four."`
	Hazard          string   `json:"hazard,omitempty" jsonschema:"Optional exact match (case-insensitive) on the published hazard, e.g. TURB, ICE, CONVECTIVE, IFR, MT_OBSC. Applied by this server; the response says so."`
	Limit           *int     `json:"limit,omitempty" jsonschema:"Optional. Return at most this many items. There is no default cap; if you set one and items are omitted, the response says how many (RESULT_TRUNCATED) and is not complete."`
	IncludeGeometry *bool    `json:"include_geometry,omitempty" jsonschema:"Optional, default true. Set false to leave geometry out of each item to make the response much smaller; the response lists geometry under omitted_fields."`
}

type weatherData struct {
	Filters       Filters          `json:"filters"`
	KindsQueried  []string         `json:"kinds_queried"`
	OmittedFields []string         `json:"omitted_fields"`
	Advisories    []model.Advisory `json:"advisories"`
}

type weatherFeed struct {
	kind string
	get  func(*awc.Client) source.Source[model.Advisory]
}

// Fixed order, so output is stable: feeds in this order, each in the source's own order.
var weatherFeeds = []weatherFeed{
	{"SIGMET", (*awc.Client).AirSigmet},
	{"AIRMET", (*awc.Client).GAirmet},
	{"INTL_SIGMET", (*awc.Client).IntlSigmet},
	{"CWA", (*awc.Client).CWA},
}

func registerWeather(srv *mcp.Server, d Deps) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "get_weather_advisories",
		Annotations: readOnly("Weather advisories (SIGMET, AIRMET, CWA)"),
		Description: d.describe("Returns the complete current set of US and international weather advisories published by the NOAA Aviation Weather Center: SIGMET, graphical AIRMET, international SIGMET and center weather advisories. " +
			"The source offers no usable area filter, so each set is returned in full, as published, with canonical GeoJSON geometry (positions are [longitude, latitude]), the advisory text as issued in raw_text where the source gives it (graphical AIRMETs publish none: their source record is in source_record), and the original validity times. " +
			"The server does not decide which advisories apply to a flight, time, altitude or place; that is for the caller." + reliance),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in WeatherInput) (*mcp.CallToolResult, any, error) {
		return weatherHandler(ctx, d, in)
	})
}

func weatherHandler(ctx context.Context, d Deps, in WeatherInput) (*mcp.CallToolResult, any, error) {
	want := map[string]bool{}
	for _, k := range in.Kinds {
		k = strings.ToUpper(strings.TrimSpace(k))
		ok := false
		for _, f := range weatherFeeds {
			ok = ok || f.kind == k
		}
		if !ok {
			names := make([]string, len(weatherFeeds))
			for i, f := range weatherFeeds {
				names[i] = f.kind
			}
			return toolError("kinds: %q is not one of %s", k, strings.Join(names, ", "))
		}
		want[k] = true
	}
	var feeds []weatherFeed
	for _, f := range weatherFeeds {
		if len(want) == 0 || want[f.kind] {
			feeds = append(feeds, f)
		}
	}
	hazard := strings.TrimSpace(in.Hazard)
	if len(hazard) > 40 {
		return toolError("hazard: too long")
	}
	if err := checkLimit(in.Limit); err != nil {
		return toolError("%v", err)
	}

	type result struct {
		snap source.Snapshot[model.Advisory]
		err  error
	}
	res := make([]result, len(feeds))
	parallel(len(feeds), func(i int) {
		res[i].snap, res[i].err = feeds[i].get(d.AWC).Fetch(ctx)
	})

	now := d.Now().UTC()
	data := weatherData{Filters: newFilters(), Advisories: []model.Advisory{}, KindsQueried: []string{}, OmittedFields: []string{}}
	if hazard != "" {
		data.Filters.AppliedByServer["hazard"] = hazard
	}
	var sources []provenance.Source
	var issues []provenance.Issue
	failed := 0
	for i, f := range feeds {
		data.KindsQueried = append(data.KindsQueried, f.kind)
		r := res[i]
		if r.err != nil {
			s, is := failure(r.snap.Source, f.get(d.AWC).ID(), r.err, now)
			sources, issues = append(sources, s), append(issues, is)
			failed++
			continue
		}
		for _, a := range r.snap.Items {
			if hazard == "" || strings.EqualFold(a.Hazard, hazard) {
				data.Advisories = append(data.Advisories, a)
			}
		}
		sources = append(sources, r.snap.Source)
		issues = append(issues, r.snap.Issues...)
	}

	// The caller's explicit shaping choices.
	if in.Limit != nil && len(data.Advisories) > *in.Limit {
		issues = append(issues, truncationIssue(len(data.Advisories), *in.Limit))
		data.Advisories = data.Advisories[:*in.Limit]
	}
	if in.IncludeGeometry != nil && !*in.IncludeGeometry {
		for i := range data.Advisories {
			data.Advisories[i].Geometry, data.Advisories[i].GeometryDerivation = nil, nil
		}
		data.OmittedFields = append(data.OmittedFields, "geometry")
	}
	// Per-source counts reflect what is actually in the response.
	returned := map[string]int{}
	for _, a := range data.Advisories {
		returned[a.Source]++
	}
	for i := range sources {
		sources[i].RecordsReturned = returned[sources[i].ID]
	}

	return respond(d, data, now, sources, nil, issues, failed == len(feeds))
}
