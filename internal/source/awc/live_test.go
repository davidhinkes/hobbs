//go:build live

package awc

import (
	"context"
	"testing"
	"time"

	"github.com/davidhinkes/hobbs/internal/model"
	"github.com/davidhinkes/hobbs/internal/provenance"
	"github.com/davidhinkes/hobbs/internal/source"
)

// TestLive exercises the real AWC API. It is built only with -tags live, so
// the normal suite never touches the network.
func TestLive(t *testing.T) {
	c := New()
	for _, s := range []source.Source[model.Advisory]{c.AirSigmet(), c.GAirmet(), c.IntlSigmet(), c.CWA()} {
		t.Run(s.ID(), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			snap, err := s.Fetch(ctx)
			if err != nil {
				t.Fatalf("fetch: %v", err)
			}
			for _, a := range snap.Items {
				if err := a.Validate(); err != nil {
					t.Errorf("%s: %v", a.ID, err)
				}
			}
			env, err := provenance.New(snap.Items, time.Now(), []provenance.Source{snap.Source}, nil, snap.Issues)
			if err != nil {
				t.Fatalf("envelope: %v", err)
			}
			t.Logf("%s: HTTP %d, %d records, status=%s, issues=%d", s.ID(), snap.Source.HTTPStatus,
				len(snap.Items), env.Completeness.Status, len(env.Completeness.Issues))
			for _, is := range env.Completeness.Issues {
				t.Logf("  [%s] %s %s", is.Severity, is.Code, is.Message)
			}
		})
	}
}

// TestLiveStations exercises the real METAR and TAF endpoints with AWC's ids
// filter. KLAX publishes both, so it must come back with records; KXYZ is not a
// station, so it must be flagged STATION_NO_DATA, never silently missing.
func TestLiveStations(t *testing.T) {
	c := New()
	fetches := map[string]func(context.Context, []string) (source.Snapshot[model.StationReport], error){
		IDMETAR: c.METARs,
		IDTAF:   c.TAFs,
	}
	for id, fetch := range fetches {
		t.Run(id, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			snap, err := fetch(ctx, []string{"KLAX", "KXYZ"})
			if err != nil {
				t.Fatalf("fetch: %v", err)
			}
			got := false
			for _, r := range snap.Items {
				if err := r.Validate(); err != nil {
					t.Errorf("%s: %v", r.ID, err)
				}
				got = got || r.Station == "KLAX"
			}
			if !got {
				t.Errorf("no record for KLAX")
			}
			flagged := false
			for _, is := range snap.Issues {
				if is.Code == provenance.StationNoData {
					for _, r := range is.Refs {
						flagged = flagged || r == "KXYZ"
					}
				}
			}
			if !flagged {
				t.Errorf("KXYZ not flagged STATION_NO_DATA: %+v", snap.Issues)
			}
			t.Logf("%s: HTTP %d, %d records, %d issues", id, snap.Source.HTTPStatus, len(snap.Items), len(snap.Issues))
		})
	}
}
