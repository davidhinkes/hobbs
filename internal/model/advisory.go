package model

import (
	"encoding/json"
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

	// Geometry is the canonical RFC 7946 geometry; nil when no spatial extent
	// could be determined. GeometryDerivation lists every translation applied
	// to produce it.
	Geometry           *Geometry   `json:"geometry,omitempty"`
	GeometryDerivation *Derivation `json:"geometry_derivation,omitempty"`

	// Details holds upstream fields with no normalized home (movement, due-to,
	// freezing-level range, ...), kept as the upstream stated them.
	Details map[string]string `json:"details,omitempty"`

	// RawText is the advisory text as issued, unaltered; RawFormat is then
	// advisory_text. Both are empty when the source published no text
	// (G-AIRMETs).
	RawText   string    `json:"raw_text,omitempty"`
	RawFormat RawFormat `json:"raw_format,omitempty"`
	// SourceRecord is the source's own record, verbatim, when there is no
	// published text or the record could not be parsed.
	SourceRecord json.RawMessage `json:"source_record,omitempty"`

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
	case a.ValidFrom != nil && a.ValidTo != nil && a.ValidTo.Before(*a.ValidFrom):
		return fmt.Errorf("advisory %s: valid_to before valid_from", a.ID)
	}
	if err := validateText("advisory", a.ID, a.RawText, a.RawFormat, []RawFormat{RawAdvisoryText}, a.SourceRecord); err != nil {
		return err
	}
	for _, alt := range []Altitude{a.Lower, a.Upper} {
		if err := alt.Validate(); err != nil {
			return fmt.Errorf("advisory %s: %w", a.ID, err)
		}
	}
	if a.Geometry != nil {
		if err := a.Geometry.Validate(); err != nil {
			return fmt.Errorf("advisory %s: %w", a.ID, err)
		}
	}
	return nil
}
