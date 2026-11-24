package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/davidhinkes/hobbs/internal/model"
	"github.com/davidhinkes/hobbs/internal/provenance"
	"github.com/davidhinkes/hobbs/internal/source"
	"github.com/davidhinkes/hobbs/internal/source/nms"
)

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

// notamResult is what collectNotams gathered: the merged NOTAMs and everything
// the provenance envelope needs. It applies no caller shaping (such as
// leaving out geometry); callers do that on NOTAMs.
type notamResult struct {
	NOTAMs       []model.Notam     // merged in query order, each NOTAM once, ReturnedFor set
	Locations    *locationsInfo    // nil when the call used no locations
	Applied      map[string]string // filters the FAA applied, including "location"; nil when the source is not enabled
	Sources      []provenance.Source
	Issues       []provenance.Issue
	NotConsulted []provenance.NotConsulted
	Failed       int // queries that failed
	Queries      int // queries made; 0 when the source is not enabled
}

// collectNotams asks the FAA once per location (or once, when locs is [""]),
// optionally adds the en-route centers the FAA lists on those NOTAMs and asks
// for them too, and merges the results so a NOTAM returned by several queries
// appears once. Per-source RecordsReturned counts the merged records. With
// d.NMS nil it returns the not-enabled entry and critical issue and makes no
// query. The error is non-nil only for nms.ErrBadQuery.
func collectNotams(ctx context.Context, d Deps, base nms.Query, locs []string, addCenters bool) (notamResult, error) {
	now := d.Now().UTC()
	res := notamResult{NOTAMs: []model.Notam{}}
	if locs[0] != "" {
		res.Locations = &locationsInfo{Requested: locs, Added: []addedCenter{}, Note: centersNotInferred}
	}

	if d.NMS == nil {
		// The NOTAM source is not enabled: say so rather than return an empty list.
		res.NotConsulted = []provenance.NotConsulted{{ID: nms.SourceID, Reason: notEnabled}}
		res.Issues = []provenance.Issue{{
			Code: provenance.SourceNotEnabled, Severity: provenance.Critical, Source: nms.SourceID,
			Message: notEnabledText,
		}}
		return res, nil
	}

	fetches := run(ctx, d.NMS, base, locs)
	var issues []provenance.Issue
	if res.Locations != nil && addCenters {
		added, missing := impliedCenters(fetches, locs)
		res.Locations.Added = added
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
				return notamResult{}, f.err
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
				i = len(res.NOTAMs)
				index[k] = i
				res.NOTAMs = append(res.NOTAMs, n)
			}
			if f.location != "" {
				res.NOTAMs[i].ReturnedFor = append(res.NOTAMs[i].ReturnedFor, f.location)
			}
		}
	}

	res.Applied = base.Applied()
	if res.Locations != nil {
		all := locs
		for _, a := range res.Locations.Added {
			all = append(all, a.Location)
		}
		res.Applied["location"] = strings.Join(all, ",")
	}

	// Each successful query's source, with how many of its records are in
	// the merged result.
	returned := map[string]int{}
	for _, n := range res.NOTAMs {
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
			s.RecordsReturned = len(res.NOTAMs)
		}
		sources = append(sources, s)
	}
	res.Sources, res.Issues, res.Failed, res.Queries = sources, issues, failed, len(fetches)
	return res, nil
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
