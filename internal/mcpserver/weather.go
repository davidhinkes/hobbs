package mcpserver

import (
	"context"

	"github.com/davidhinkes/hobbs/internal/model"
	"github.com/davidhinkes/hobbs/internal/provenance"
	"github.com/davidhinkes/hobbs/internal/source"
	"github.com/davidhinkes/hobbs/internal/source/awc"
)

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

// advisoryResult is what collectAdvisories gathered, before any caller filter.
type advisoryResult struct {
	ByKind  map[string][]model.Advisory // records per feed kind, in the source's order; absent for a feed that failed
	Sources []provenance.Source         // in feed order, failed feeds included; RecordsReturned is not set
	Issues  []provenance.Issue
	Failed  int // feeds that failed
	Queries int // feeds asked
}

// collectAdvisories fetches the given feeds in parallel from the AWC. A feed
// that fails gets a source entry and an issue and no records; the others are
// unaffected.
func collectAdvisories(ctx context.Context, d Deps, feeds []weatherFeed) advisoryResult {
	type result struct {
		snap source.Snapshot[model.Advisory]
		err  error
	}
	res := make([]result, len(feeds))
	parallel(len(feeds), func(i int) {
		res[i].snap, res[i].err = feeds[i].get(d.AWC).Fetch(ctx)
	})

	now := d.Now().UTC()
	out := advisoryResult{ByKind: map[string][]model.Advisory{}, Queries: len(feeds)}
	for i, f := range feeds {
		r := res[i]
		if r.err != nil {
			s, is := failure(r.snap.Source, f.get(d.AWC).ID(), r.err, now)
			out.Sources, out.Issues = append(out.Sources, s), append(out.Issues, is)
			out.Failed++
			continue
		}
		out.ByKind[f.kind] = r.snap.Items
		out.Sources = append(out.Sources, r.snap.Source)
		out.Issues = append(out.Issues, r.snap.Issues...)
	}
	return out
}
