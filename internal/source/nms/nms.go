// Package nms will adapt the FAA NOTAM Management Service API.
//
// The NMS-API is not publicly documented and needs credentials from the FAA
// (request via the link on faa.gov/about/initiatives/notam). Until access is
// granted this adapter is disabled: it fetches nothing, and responses report
// the source as not consulted so results are marked incomplete.
package nms

import (
	"context"

	"github.com/davidhinkes/hobbs/internal/model"
	"github.com/davidhinkes/hobbs/internal/source"
)

// SourceID identifies this source in provenance.
const SourceID = "faa.nms.notam"

// Disabled is the placeholder adapter used until FAA access exists.
type Disabled struct{}

var _ source.Source[model.Notam] = Disabled{}

func (Disabled) ID() string { return SourceID }

func (Disabled) Fetch(context.Context) (source.Snapshot[model.Notam], error) {
	return source.Snapshot[model.Notam]{}, source.ErrNotEnabled
}
