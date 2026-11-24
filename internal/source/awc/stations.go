package awc

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/davidhinkes/hobbs/internal/model"
	"github.com/davidhinkes/hobbs/internal/provenance"
	"github.com/davidhinkes/hobbs/internal/source"
)

// stationNoDataMessage is the text of the STATION_NO_DATA issue. AWC gives one
// answer for three different situations, so the message names all of them and
// the issue is never to be read as a confirmed "none".
const stationNoDataMessage = "AWC returned no report for these requested station(s). AWC answers the same way for " +
	`"no current report", "no TAF published here" and "unknown station", so this does not say which applies`

// METARs fetches the current METAR and SPECI reports of the given stations
// (ICAO identifiers) with AWC's own "ids" filter, in the order given. Every
// record AWC returns is kept, including stations that were not requested.
// Requested stations with no record are listed in a STATION_NO_DATA issue.
// On failure the snapshot's Source is filled as far as the fetch got and the
// error wraps ErrUpstream.
func (c *Client) METARs(ctx context.Context, stations []string) (source.Snapshot[model.StationReport], error) {
	return c.fetchStations(ctx, IDMETAR, "metar", model.KindMETAR, stations)
}

// TAFs is METARs for terminal aerodrome forecasts.
func (c *Client) TAFs(ctx context.Context, stations []string) (source.Snapshot[model.StationReport], error) {
	return c.fetchStations(ctx, IDTAF, "taf", model.KindTAF, stations)
}

// fetchStations is the shared fetch behind METARs and TAFs: one request for
// all stations, one StationReport per upstream record (a record that cannot be
// parsed or identified is kept as a flagged stub, never dropped), and the
// PARSE_FAILED and STATION_NO_DATA issues.
func (c *Client) fetchStations(ctx context.Context, id, path string, kind model.ReportKind, stations []string) (source.Snapshot[model.StationReport], error) {
	u := c.BaseURL + "/" + path + "?ids=" + url.QueryEscape(strings.Join(stations, ",")) + "&format=json"
	u = strings.ReplaceAll(u, "%2C", ",")
	src, recs, err := c.getJSON(ctx, id, u)
	snap := source.Snapshot[model.StationReport]{Source: src}
	if err != nil {
		return snap, err
	}

	parse := parseMETAR
	if kind == model.KindTAF {
		parse = parseTAF
	}
	var (
		parseFail []string
		have      = map[string]bool{}
		seen      = map[string]int{}
		newest    time.Time
	)
	for i, raw := range recs {
		r, n := parse(id, raw)
		if r.Station == "" {
			// A stub: keep what was parsed (its text included), and identify it
			// by its position in the answer.
			n.Add("record has no station identifier")
			r.ID, r.Kind, r.Source = fmt.Sprintf("%s#%d", id, i), kind, id
			r.Station = r.ID
		} else {
			have[strings.ToUpper(r.Station)] = true
		}
		if r.RawText == "" {
			r.RawFormat = ""
			n.Add("official text missing")
		}
		if r.RawText == "" || len(n.Issues) > 0 {
			r.SourceRecord = model.SourceRecord(raw)
		}
		if c := seen[r.ID]; c > 0 {
			seen[r.ID]++
			r.ID = fmt.Sprintf("%s #%d", r.ID, c+1)
		} else {
			seen[r.ID] = 1
		}
		r.ParseIssues = n.Issues
		if len(n.Issues) > 0 {
			parseFail = append(parseFail, r.ID)
		}
		if t := r.ObservedAt; t != nil && t.After(newest) {
			newest = *t
		}
		if t := r.IssuedAt; t != nil && t.After(newest) {
			newest = *t
		}
		if err := r.Validate(); err != nil {
			return snap, fmt.Errorf("%w: %s: internal: record %d failed validation: %v", ErrUpstream, id, i, err)
		}
		snap.Items = append(snap.Items, r)
	}
	if !newest.IsZero() {
		snap.Source.UpstreamTimestamp = &newest
	}
	snap.Source.RecordsReturned = len(snap.Items)

	snap.Issues = source.AppendIssue(snap.Issues, id, provenance.ParseFailed, provenance.Warning, parseFail,
		fmt.Sprintf("%d record(s) had fields that could not be parsed; they are included, flagged in parse_issues", len(parseFail)))

	var missing []string
	asked := map[string]bool{}
	for _, s := range stations {
		s = strings.ToUpper(strings.TrimSpace(s))
		if s != "" && !have[s] && !asked[s] {
			missing = append(missing, s)
		}
		asked[s] = true
	}
	if len(missing) > 0 {
		// Keep request order: AppendIssue sorts, so build the issue directly
		// unless the list is long enough to need its truncation note.
		is := source.AppendIssue(nil, id, provenance.StationNoData, provenance.Warning, missing, stationNoDataMessage)
		if len(missing) <= source.MaxRefs {
			is[0].Refs = missing
		}
		snap.Issues = append(snap.Issues, is...)
	}
	return snap, nil
}

// stationFields reads one AWC record field by field, so that one bad field
// is noted without losing the others.
type stationFields struct {
	m map[string]json.RawMessage
	n *note
}

func (f stationFields) get(key string, dst any) bool {
	b, ok := f.m[key]
	if !ok || string(b) == "null" {
		return false
	}
	if err := json.Unmarshal(b, dst); err != nil {
		f.n.Add("field %s: %v", key, err)
		return false
	}
	return true
}

func (f stationFields) time(key string) *time.Time {
	var t flexTime
	if f.get(key, &t) {
		return tptr(&t)
	}
	return nil
}

func (f stationFields) text(key string) string {
	var s flexString
	f.get(key, &s)
	return string(s)
}

// common reads the fields METAR and TAF records share: station, name,
// elevation (metres) and position, and puts every other field into Details.
func (f stationFields) common(r *model.StationReport, mapped []string) {
	r.Station = strings.TrimSpace(f.text("icaoId"))
	r.StationName = f.text("name")
	var elev flexFloat
	if f.get("elev", &elev) {
		v := float64(elev)
		r.ElevationM = &v
	}
	var lat, lon flexFloat
	hasLat, hasLon := f.get("lat", &lat), f.get("lon", &lon)
	switch {
	case hasLat && hasLon:
		g := model.NewPoint(float64(lon), float64(lat))
		if err := g.Validate(); err != nil {
			f.n.Add("position: %v", err)
		} else {
			r.Geometry = &g
		}
	case hasLat != hasLon:
		f.n.Add("position: only one of lat/lon present")
	}
	rest := map[string]json.RawMessage{}
	skip := map[string]bool{}
	for _, k := range mapped {
		skip[k] = true
	}
	for k, v := range f.m {
		if !skip[k] {
			rest[k] = v
		}
	}
	if len(rest) > 0 {
		r.Details, _ = json.Marshal(rest)
	}
}

func newStationFields(raw json.RawMessage) (stationFields, *note) {
	n := &note{}
	f := stationFields{m: map[string]json.RawMessage{}, n: n}
	if err := json.Unmarshal(raw, &f.m); err != nil {
		n.Add("record is not a JSON object: %v", err)
	}
	return f, n
}

func stationID(prefix, station string, t *time.Time) string {
	id := prefix + " " + station
	if t != nil {
		id += " " + t.Format(time.RFC3339)
	}
	return id
}

// parseMETAR maps one AWC METAR record. Mapped keys: icaoId, name, elev, lat,
// lon, metarType, obsTime, reportTime, receiptTime, rawOb; the rest of the
// record is kept as Details. The ID is "<type> <station> <observation time>".
func parseMETAR(src string, raw json.RawMessage) (model.StationReport, *note) {
	f, n := newStationFields(raw)
	r := model.StationReport{Kind: model.KindMETAR, Source: src}
	f.common(&r, []string{"icaoId", "name", "elev", "lat", "lon", "metarType", "obsTime", "reportTime", "receiptTime", "rawOb"})
	r.ReportType = f.text("metarType")
	r.ObservedAt = f.time("obsTime")
	r.ReportedAt = f.time("reportTime")
	r.ReceivedAt = f.time("receiptTime")
	r.RawText = f.text("rawOb")
	if r.RawText != "" {
		r.RawFormat = model.RawMETAR
	}
	if r.Station != "" {
		typ := r.ReportType
		if typ == "" {
			typ = "METAR"
		}
		r.ID = stationID(typ, r.Station, r.ObservedAt)
	}
	return r, n
}

// parseTAF maps one AWC TAF record. Mapped keys: icaoId, name, elev, lat, lon,
// issueTime, validTimeFrom, validTimeTo, rawTAF; the rest (including the
// decoded forecast periods, fcsts) is kept as Details. The ID is
// "TAF <station> <issue time>".
func parseTAF(src string, raw json.RawMessage) (model.StationReport, *note) {
	f, n := newStationFields(raw)
	r := model.StationReport{Kind: model.KindTAF, Source: src}
	f.common(&r, []string{"icaoId", "name", "elev", "lat", "lon", "issueTime", "validTimeFrom", "validTimeTo", "rawTAF"})
	r.IssuedAt = f.time("issueTime")
	r.ValidFrom = f.time("validTimeFrom")
	r.ValidTo = f.time("validTimeTo")
	if r.ValidFrom != nil && r.ValidTo != nil && r.ValidTo.Before(*r.ValidFrom) {
		n.Add("valid_to precedes valid_from; validity window discarded")
		r.ValidFrom, r.ValidTo = nil, nil
	}
	r.RawText = f.text("rawTAF")
	if r.RawText != "" {
		r.RawFormat = model.RawTAF
	}
	if r.Station != "" {
		r.ID = stationID("TAF", r.Station, r.IssuedAt)
	}
	return r, n
}
