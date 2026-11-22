package model

import (
	"errors"
	"fmt"
	"time"
)

// Notam is a normalized NOTAM. It uses only standard ICAO NOTAM concepts so it
// does not depend on any one upstream's schema. RawText is the official
// text exactly as received and is never altered or summarized.
type Notam struct {
	// ID is the NOTAM identifier as issued, e.g. "FDC 6/9306" or "A1234/26".
	ID string `json:"id"`
	// Type is N (new), R (replace) or C (cancel) when known, else empty.
	Type string `json:"type,omitempty"`
	// Replaces is the ID this NOTAM replaces or cancels, when stated.
	Replaces string `json:"replaces,omitempty"`

	// Location is the affected location/ICAO identifier(s) as stated.
	Location string `json:"location,omitempty"`
	FIR      string `json:"fir,omitempty"`
	// QCode is the ICAO Q-code (item Q), when present.
	QCode string `json:"q_code,omitempty"`

	// EffectiveFrom is nil only if the upstream gave none. EffectiveTo is nil
	// for permanent or unstated; check Permanent and Estimated.
	EffectiveFrom *time.Time `json:"effective_from,omitempty"`
	EffectiveTo   *time.Time `json:"effective_to,omitempty"`
	Permanent     bool       `json:"permanent,omitempty"`
	Estimated     bool       `json:"estimated,omitempty"` // end time marked EST
	// Schedule is item D (daily/periodic activity) as stated. It is not parsed
	// here; a non-empty value means "active now" cannot be decided from
	// EffectiveFrom/EffectiveTo alone.
	Schedule string `json:"schedule,omitempty"`

	Lower Altitude `json:"lower"`
	Upper Altitude `json:"upper"`

	// Center and RadiusNM come from the Q line, when present.
	Center   *LatLon  `json:"center,omitempty"`
	RadiusNM *float64 `json:"radius_nm,omitempty"`

	// Geometry is nil when the NOTAM has no spatial extent we could determine.
	// Consumers must then treat spatial applicability as unknown.
	Geometry *Geometry `json:"geometry,omitempty"`

	// Text is the NOTAM body (item E) as stated.
	Text string `json:"text,omitempty"`
	// RawText is the complete official text, unaltered. Always present.
	RawText string `json:"raw_text"`

	// ParseIssues lists fields that could not be parsed. A record with parse
	// issues is still returned, never dropped.
	ParseIssues []string `json:"parse_issues,omitempty"`
}

// LatLon is a WGS84 position in signed decimal degrees.
type LatLon struct {
	Lat float64 `json:"lat"`
	Lon float64 `json:"lon"`
}

// Geometry is a spatial extent: a closed ring (polygon), several rings
// (a multi-part area), an open line, or a circle. Exactly one form is set. A
// line has no inside and must not be treated as an area.
//
// Longitudes are as the upstream published them. For features that cross the
// antimeridian that can be outside [-180, 180] (e.g. 196.3 for 163.7W): the
// unwrapped form keeps each ring continuous, and geometry code must account
// for it.
type Geometry struct {
	Ring     []LatLon   `json:"ring,omitempty"`
	Rings    [][]LatLon `json:"rings,omitempty"`
	Line     []LatLon   `json:"line,omitempty"`
	Center   *LatLon    `json:"center,omitempty"`
	RadiusNM *float64   `json:"radius_nm,omitempty"`
}

// Validate checks the invariants every NOTAM must satisfy before it is
// served: an ID, the untouched raw text, sane time ordering and altitudes.
func (n Notam) Validate() error {
	if n.ID == "" {
		return errors.New("notam: id is required")
	}
	if n.RawText == "" {
		return fmt.Errorf("notam %s: raw_text is required", n.ID)
	}
	if n.EffectiveFrom != nil && n.EffectiveTo != nil && n.EffectiveTo.Before(*n.EffectiveFrom) {
		return fmt.Errorf("notam %s: effective_to before effective_from", n.ID)
	}
	if n.Permanent && n.EffectiveTo != nil {
		return fmt.Errorf("notam %s: permanent NOTAM must not have effective_to", n.ID)
	}
	for _, a := range []Altitude{n.Lower, n.Upper} {
		if err := a.Validate(); err != nil {
			return fmt.Errorf("notam %s: %w", n.ID, err)
		}
	}
	return nil
}

// ActiveAt reports whether the NOTAM is within its effective period at t,
// using a half-open interval [from, to). The second result is false when
// the answer cannot be determined from the effective period alone (no start
// time, or a non-empty Schedule), in which case callers must treat the NOTAM
// as possibly active and flag it.
func (n Notam) ActiveAt(t time.Time) (active bool, determinate bool) {
	if n.EffectiveFrom == nil || n.Schedule != "" {
		return true, false
	}
	if t.Before(*n.EffectiveFrom) {
		return false, true
	}
	if n.Permanent || n.EffectiveTo == nil {
		// Open-ended: active, but an unstated end is only determinate if permanent.
		return true, n.Permanent
	}
	return t.Before(*n.EffectiveTo), true
}
