// Package source defines the contract every upstream adapter implements.
package source

import (
	"context"
	"errors"

	"github.com/davidhinkes/hobbs/internal/provenance"
)

// ErrNotEnabled is returned by an adapter that is configured off, for
// example the NOTAM adapter before FAA credentials exist.
var ErrNotEnabled = errors.New("source not enabled")

// Snapshot is one fetch of a source: the parsed items plus everything needed
// to attribute them. Source.RetrievedAt, HTTP status, ETag and hash describe
// the fetch; Issues carry parse failures and other data-pull problems.
type Snapshot[T any] struct {
	Items  []T
	Source provenance.Source
	Issues []provenance.Issue
}

// Source is an official upstream adapter. Fetch returns the whole feed
// (upstream filters are unreliable); filtering is done by the caller. An
// error means no usable data was obtained. Partial parse failures belong in
// Snapshot.Issues, not in the error.
type Source[T any] interface {
	ID() string
	Fetch(ctx context.Context) (Snapshot[T], error)
}

// NotEnabled builds the not-consulted entry for a disabled source, so a
// response states what was skipped and is marked incomplete.
func NotEnabled(id string) provenance.NotConsulted {
	return provenance.NotConsulted{ID: id, Reason: "not_enabled"}
}
