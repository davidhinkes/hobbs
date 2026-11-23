package mcpserver

import (
	"context"
	"errors"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/davidhinkes/hobbs/internal/model"
	"github.com/davidhinkes/hobbs/internal/provenance"
	"github.com/davidhinkes/hobbs/internal/source/nms"
)

// NotamsInput is the input of get_notams. Every filter is forwarded to the
// FAA NMS-API, which applies it; the server adds none of its own.
type NotamsInput struct {
	Location        string   `json:"location,omitempty" jsonschema:"ICAO or domestic location identifier, e.g. KIAD or IAD."`
	Latitude        *float64 `json:"latitude,omitempty" jsonschema:"Decimal degrees, -90 to 90. Must be given together with longitude and radius_nm."`
	Longitude       *float64 `json:"longitude,omitempty" jsonschema:"Decimal degrees, -180 to 180. Must be given together with latitude and radius_nm."`
	RadiusNM        *float64 `json:"radius_nm,omitempty" jsonschema:"Search radius in nautical miles, 0 to 100 (the API's limit). Must be given together with latitude and longitude."`
	EffectiveStart  string   `json:"effective_start,omitempty" jsonschema:"RFC 3339 UTC timestamp. Must be given together with effective_end. The API returns NOTAMs effective in the window."`
	EffectiveEnd    string   `json:"effective_end,omitempty" jsonschema:"RFC 3339 UTC timestamp. Must be given together with effective_start."`
	LastUpdated     string   `json:"last_updated,omitempty" jsonschema:"RFC 3339 UTC timestamp within the previous 24 hours; returns NOTAMs created, updated or cancelled since then, active or not."`
	Classification  string   `json:"classification,omitempty" jsonschema:"One of DOMESTIC, INTERNATIONAL, MILITARY, LOCAL_MILITARY, FDC. Needs another filter alongside it. Note records report the short form (DOM, INTL, MIL, FDC)."`
	Feature         string   `json:"feature,omitempty" jsonschema:"NOTAM feature: RWY, TWY, APRON, AD, OBST, NAV, COM, SVC, AIRSPACE, ODP, SID, STAR, CHART, DATA, DVA, IAP, VFP, ROUTE, SPECIAL or SECURITY."`
	Accountability  string   `json:"accountability,omitempty" jsonschema:"Accountability code, e.g. FDC or an airport id."`
	FreeText        string   `json:"free_text,omitempty" jsonschema:"Free-text search, evaluated by the API."`
	NotamNumber     string   `json:"notam_number,omitempty" jsonschema:"NOTAM number, e.g. 08/224 or A4429/26. Needs location or accountability."`
	Limit           *int     `json:"limit,omitempty" jsonschema:"Optional. Return at most this many items. There is no default cap; if you set one and items are omitted, the response says how many (RESULT_TRUNCATED) and is not complete."`
	IncludeGeometry *bool    `json:"include_geometry,omitempty" jsonschema:"Optional, default true. Set false to leave geometry out of each item to make the response much smaller; the response lists geometry under omitted_fields."`
}

type notamData struct {
	Filters       Filters       `json:"filters"`
	OmittedFields []string      `json:"omitted_fields"`
	NOTAMs        []model.Notam `json:"notams"`
}

func registerNotams(srv *mcp.Server, d Deps) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "get_notams",
		Annotations: readOnly("NOTAMs from the FAA NOTAM Management Service"),
		Description: d.describe("Returns NOTAMs from the FAA NOTAM Management Service (NMS-API), including FDC NOTAMs. " +
			"You must give at least one filter (location, a latitude/longitude/radius_nm area, an effective window, or others); the filters are applied by the FAA's API, not by this server, and the response lists exactly which. " +
			"Each NOTAM carries its raw text in every rendering the FAA supplied (ICAO and local format), the published fields, and canonical GeoJSON geometry where one is published (positions are [longitude, latitude]; a point with a radius is translated to a slightly larger circle polygon, with the original circle in geometry_derivation). " +
			"Results can be large (an airport can have hundreds of NOTAMs); nothing is ranked, summarized or dropped. " +
			"NOTAMs for temporary flight restrictions appear here as FDC NOTAMs with their text and dates, but the FAA's TFR outlines are not available from this source, so geometry for them may be missing or only a point. " + reliance),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in NotamsInput) (*mcp.CallToolResult, any, error) {
		return notamsHandler(ctx, d, in)
	})
}

func notamsHandler(ctx context.Context, d Deps, in NotamsInput) (*mcp.CallToolResult, any, error) {
	q := nms.Query{
		Location: strings.TrimSpace(in.Location), Latitude: in.Latitude, Longitude: in.Longitude, RadiusNM: in.RadiusNM,
		Classification: strings.TrimSpace(in.Classification), Feature: strings.TrimSpace(in.Feature),
		Accountability: strings.TrimSpace(in.Accountability), FreeText: strings.TrimSpace(in.FreeText), NotamNumber: strings.TrimSpace(in.NotamNumber),
	}
	if err := checkLimit(in.Limit); err != nil {
		return toolError("%v", err)
	}
	var err error
	if q.EffectiveStart, err = parseTime("effective_start", in.EffectiveStart); err != nil {
		return toolError("%v", err)
	}
	if q.EffectiveEnd, err = parseTime("effective_end", in.EffectiveEnd); err != nil {
		return toolError("%v", err)
	}
	if q.LastUpdated, err = parseTime("last_updated", in.LastUpdated); err != nil {
		return toolError("%v", err)
	}

	now := d.Now().UTC()
	data := notamData{Filters: newFilters(), NOTAMs: []model.Notam{}, OmittedFields: []string{}}

	if d.NMS == nil {
		// Credentials are not configured: say so rather than return an empty list.
		env, perr := provenance.New(data, now, nil, []provenance.NotConsulted{{ID: nms.SourceID, Reason: "not_configured"}}, d.mark([]provenance.Issue{{
			Code: provenance.SourceNotEnabled, Severity: provenance.Critical, Source: nms.SourceID,
			Message: "NOTAMs are unavailable: the NOTAM source is not enabled on this server",
		}}))
		if perr != nil {
			return toolError("internal error: %v", perr)
		}
		env.Airworthy = d.Airworthy
		return envelopeResult(env, true)
	}

	snap, qerr := d.NMS.Query(ctx, q)
	if qerr != nil {
		if errors.Is(qerr, nms.ErrBadQuery) {
			return toolError("%v", qerr) // the request itself is wrong; the agent can fix it
		}
		s, is := failure(snap.Source, nms.SourceID, qerr, now)
		env, perr := provenance.New(data, now, []provenance.Source{s}, nil, d.mark([]provenance.Issue{is}))
		if perr != nil {
			return toolError("internal error: %v", perr)
		}
		env.Airworthy = d.Airworthy
		return envelopeResult(env, true)
	}

	data.Filters.AppliedBySource = q.Applied()
	data.NOTAMs = append(data.NOTAMs, snap.Items...)
	issues := snap.Issues
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
	snap.Source.RecordsReturned = len(data.NOTAMs)
	env, perr := provenance.New(data, now, []provenance.Source{snap.Source}, nil, d.mark(issues))
	if perr != nil {
		return toolError("internal error: %v", perr)
	}
	env.Airworthy = d.Airworthy
	return envelopeResult(env, false)
}
