package model

import (
	"errors"
	"fmt"
	"time"
)

// AdvisoryKind is the type of weather advisory.
type AdvisoryKind string

const (
	KindSIGMET     AdvisoryKind = "SIGMET"      // US SIGMET, including convective
	KindAIRMET     AdvisoryKind = "AIRMET"      // US AIRMET (G-AIRMET snapshots)
	KindIntlSIGMET AdvisoryKind = "INTL_SIGMET" // international SIGMET
	KindCWA        AdvisoryKind = "CWA"         // Center Weather Advisory
)

// RawFormat says what RawText contains.
type RawFormat string

const (
	// RawOfficialText is the official advisory text as issued.
	RawOfficialText RawFormat = "official_text"
	// RawUpstreamJSON is the upstream's JSON record verbatim, used when the
	// upstream publishes no text (G-AIRMET) or the record could not be parsed.
	RawUpstreamJSON RawFormat = "upstream_json"
)

// Advisory is a normalized weather advisory (SIGMET, AIRMET, CWA).
type Advisory struct {
	ID     string       `json:"id"`
	Kind   AdvisoryKind `json:"kind"`
	Source string       `json:"source"` // provenance source id, e.g. "awc.gairmet"
	Issuer string       `json:"issuer,omitempty"`

	Hazard    string `json:"hazard,omitempty"`
	Severity  string `json:"severity,omitempty"`
	Qualifier string `json:"qualifier,omitempty"`

	IssuedAt  *time.Time `json:"issued_at,omitempty"`
	ValidFrom *time.Time `json:"valid_from,omitempty"`
	ValidTo   *time.Time `json:"valid_to,omitempty"`

	Lower Altitude `json:"lower"`
	Upper Altitude `json:"upper"`

	// Geometry is nil when no spatial extent could be determined; consumers
	// must then treat spatial applicability as unknown.
	Geometry *Geometry `json:"geometry,omitempty"`

	// Details holds upstream fields with no normalized home (movement, due-to,
	// freezing-level range, ...), kept as the upstream stated them.
	Details map[string]string `json:"details,omitempty"`

	RawText   string    `json:"raw_text"`
	RawFormat RawFormat `json:"raw_format"`

	// ParseIssues lists fields that could not be parsed. Such records are
	// still returned, never dropped.
	ParseIssues []string `json:"parse_issues,omitempty"`
}

// Validate checks the invariants every Advisory must satisfy before it is served.
func (a Advisory) Validate() error {
	switch {
	case a.ID == "":
		return errors.New("advisory: id is required")
	case a.Kind == "":
		return fmt.Errorf("advisory %s: kind is required", a.ID)
	case a.Source == "":
		return fmt.Errorf("advisory %s: source is required", a.ID)
	case a.RawText == "":
		return fmt.Errorf("advisory %s: raw_text is required", a.ID)
	case a.RawFormat != RawOfficialText && a.RawFormat != RawUpstreamJSON:
		return fmt.Errorf("advisory %s: invalid raw_format %q", a.ID, a.RawFormat)
	case a.ValidFrom != nil && a.ValidTo != nil && a.ValidTo.Before(*a.ValidFrom):
		return fmt.Errorf("advisory %s: valid_to before valid_from", a.ID)
	}
	for _, alt := range []Altitude{a.Lower, a.Upper} {
		if err := alt.Validate(); err != nil {
			return fmt.Errorf("advisory %s: %w", a.ID, err)
		}
	}
	if g := a.Geometry; g != nil {
		n := 0
		if len(g.Ring) > 0 {
			n++
		}
		if len(g.Rings) > 0 {
			n++
		}
		if len(g.Line) > 0 {
			n++
		}
		if g.Center != nil {
			n++
		}
		if n != 1 {
			return fmt.Errorf("advisory %s: geometry must set exactly one of ring, rings, line, circle", a.ID)
		}
	}
	return nil
}

// ActiveAt reports whether the advisory is within its validity window at t
// ([from, to) half-open). The second result is false when the window is
// missing a bound, in which case callers must treat it as possibly active.
func (a Advisory) ActiveAt(t time.Time) (active bool, determinate bool) {
	if a.ValidFrom == nil || a.ValidTo == nil {
		return true, false
	}
	return !t.Before(*a.ValidFrom) && t.Before(*a.ValidTo), true
}
