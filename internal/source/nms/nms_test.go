package nms

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/davidhinkes/hobbs/internal/model"
	"github.com/davidhinkes/hobbs/internal/provenance"
	"github.com/davidhinkes/hobbs/internal/source"
)

func TestDisabledFetchesNothing(t *testing.T) {
	snap, err := Disabled{}.Fetch(context.Background())
	if !errors.Is(err, source.ErrNotEnabled) {
		t.Fatalf("err = %v, want ErrNotEnabled", err)
	}
	if len(snap.Items) != 0 {
		t.Error("disabled adapter returned items")
	}
}

// A response that should have included NOTAMs but skipped the source must be
// incomplete and unable to assert "no NOTAMs".
func TestDisabledMakesResponseIncomplete(t *testing.T) {
	d := Disabled{}
	env, err := provenance.New([]model.Notam{}, time.Now(), nil,
		[]provenance.NotConsulted{source.NotEnabled(d.ID())}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if env.Completeness.Status != provenance.Incomplete || env.Completeness.CanAssertNone() {
		t.Errorf("status = %s, CanAssertNone = %v", env.Completeness.Status, env.Completeness.CanAssertNone())
	}
	if env.Provenance.NotConsulted[0].ID != "faa.nms.notam" {
		t.Errorf("not_consulted = %+v", env.Provenance.NotConsulted)
	}
}
