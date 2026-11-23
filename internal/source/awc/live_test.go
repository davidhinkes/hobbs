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
