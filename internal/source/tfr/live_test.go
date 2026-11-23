//go:build tfrlive

package tfr

import (
	"context"
	"testing"
	"time"

	"github.com/davidhinkes/hobbs/internal/model"
	"github.com/davidhinkes/hobbs/internal/provenance"
)

// TestLive exercises the real TFR site. The adapter is turned off, so this is
// built only with its own tag, -tags tfrlive (-tags live does not reach
// tfr.faa.gov).
func TestLive(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	start := time.Now()
	c := New()
	c.Enabled = true
	snap, err := c.Fetch(ctx, Filter{})
	if err != nil {
		if snap.Source.HTTPStatus == 403 {
			// tfr.faa.gov serves "Access Denied" to this client. That is the
			// site's access control; we do not work around it. See docs/SPIKE.md.
			t.Skipf("tfr.faa.gov denied this client (HTTP 403): %v", err)
		}
		t.Fatal(err)
	}
	var areas, withGeom, unknownAlt, zoneless int
	types := map[model.GeometryType]int{}
	for _, x := range snap.Items {
		if err := x.Validate(); err != nil {
			t.Errorf("%s: %v", x.ID, err)
		}
		if len(x.ParseIssues) > 0 {
			t.Logf("flagged %s (%s): %v", x.ID, x.Type, x.ParseIssues)
		}
		if x.EffectiveRaw != "" && x.Effective == nil {
			zoneless++
		}
		for _, a := range x.Areas {
			areas++
			if a.Geometry != nil {
				withGeom++
				types[a.Geometry.Type]++
			}
			if !a.Lower.Known() || !a.Upper.Known() {
				unknownAlt++
			}
		}
	}
	env, err := provenance.New(snap.Items, time.Now(), []provenance.Source{snap.Source}, nil, snap.Issues)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%d TFRs in %v; %d areas (%d with geometry %v); %d areas with an unknown limit; %d unconverted times; status=%s",
		len(snap.Items), time.Since(start).Round(time.Millisecond), areas, withGeom, types, unknownAlt, zoneless, env.Completeness.Status)
	for _, is := range env.Completeness.Issues {
		t.Logf("  [%s] %s: %s", is.Severity, is.Code, is.Message)
	}
}
