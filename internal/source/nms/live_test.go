package nms

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/davidhinkes/hobbs/internal/provenance"
)

// TestLive exercises the real NMS-API using NMS_HOST, NMS_CLIENT_ID,
// NMS_CLIENT_SECRET (and NMS_ENV). It is skipped unless HOBBS_LIVE_NMS=1 so the
// normal suite never needs credentials or the network.
func TestLive(t *testing.T) {
	if os.Getenv("HOBBS_LIVE_NMS") != "1" {
		t.Skip("set HOBBS_LIVE_NMS=1 (and NMS_* variables) to run against the NMS-API")
	}
	cfg, ok := ConfigFromEnv()
	if !ok {
		t.Fatal("NMS_HOST, NMS_CLIENT_ID and NMS_CLIENT_SECRET must be set")
	}
	c, err := NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	lat, lon, r25 := 38.95, -77.45, 25.0
	olat, olon, r5 := 0.0, -30.0, 5.0
	for name, q := range map[string]Query{
		"location KIAD":      {Location: "KIAD"},
		"radius 25nm of IAD": {Latitude: &lat, Longitude: &lon, RadiusNM: &r25},
		"empty (mid-ocean)":  {Latitude: &olat, Longitude: &olon, RadiusNM: &r5},
	} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			snap, err := c.Query(ctx, q)
			if err != nil {
				t.Fatalf("query: %v", err)
			}
			flagged := 0
			for _, a := range snap.Items {
				if err := a.Validate(); err != nil {
					t.Errorf("%s: %v", a.ID, err)
				}
				if len(a.ParseIssues) > 0 {
					flagged++
					t.Logf("flagged %s (%s): %v", a.ID, a.Classification, a.ParseIssues)
				}
			}
			env, err := provenance.New(snap.Items, time.Now(), []provenance.Source{snap.Source}, nil, snap.Issues)
			if err != nil {
				t.Fatalf("envelope: %v", err)
			}
			t.Logf("HTTP %d, %d records (%d with parse issues), status=%s", snap.Source.HTTPStatus, len(snap.Items), flagged, env.Completeness.Status)
			for _, is := range env.Completeness.Issues {
				t.Logf("  [%s] %s: %s", is.Severity, is.Code, is.Message)
			}
		})
	}
}
