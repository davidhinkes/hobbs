//go:build live

package nms

import (
	"context"
	"testing"
	"time"

	"github.com/davidhinkes/hobbs/internal/provenance"
)

// TestLive exercises the real NMS-API (staging) with the credentials in
// HOBBS_NMS_CLIENT_ID and HOBBS_NMS_CLIENT_SECRET. Built only with -tags live, and skipped
// when the credentials are not set.
func TestLive(t *testing.T) {
	id, secret, ok := CredentialsFromEnv()
	if !ok {
		t.Skip("HOBBS_NMS_CLIENT_ID and HOBBS_NMS_CLIENT_SECRET are not set")
	}
	c, err := NewClient(Config{Host: StagingHost, ClientID: id, ClientSecret: secret})
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
