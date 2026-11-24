// Package nms adapts the FAA NOTAM Management Service API (NMS-API).
//
// The API is documented by an OpenAPI spec the FAA provides with credentials
// (it is not public); access is requested via faa.gov/about/initiatives/notam.
// Client queries it (staging or production, by host); behavior follows
// observed responses (docs/DESIGN.md §5.5). With no -nms-url the server has no
// Client, and NOTAM responses report the source as not consulted, so they are
// marked incomplete.
package nms

import (
	"context"

	"github.com/davidhinkes/hobbs/internal/model"
	"github.com/davidhinkes/hobbs/internal/source"
)

// SourceID identifies this source in provenance.
const SourceID = "faa.nms.notam"

// Disabled is an adapter that fetches nothing. The server does not use it
// (it has no Client when NOTAMs are off); it is kept by the maintainer's choice.
type Disabled struct{}

var _ source.Source[model.Notam] = Disabled{}

func (Disabled) ID() string { return SourceID }

func (Disabled) Fetch(context.Context) (source.Snapshot[model.Notam], error) {
	return source.Snapshot[model.Notam]{}, source.ErrNotEnabled
}
