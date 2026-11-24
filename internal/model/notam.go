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
	// Schedule is item D (daily/periodic activity) exactly as stated. The
	// server does not interpret it.
	Schedule string `json:"schedule,omitempty"`

	Lower Altitude `json:"lower"`
	Upper Altitude `json:"upper"`

	// Geometry is the canonical RFC 7946 geometry; nil when the NOTAM has no
	// spatial extent we could determine. GeometryDerivation lists every
	// translation applied to produce it (e.g. a Q-line circle converted to a
	// polygon), so the source's own shape is always recoverable.
	Geometry           *Geometry   `json:"geometry,omitempty"`
	GeometryDerivation *Derivation `json:"geometry_derivation,omitempty"`

	// Text is the message body as the source states it in its own text field
	// (for NMS, with the domestic keyword, e.g. "SVC ..."). It is not
	// necessarily identical to ICAO item E, which omits the keyword.
	Text string `json:"text,omitempty"`
	// RawText is the complete NOTAM text as the source rendered it, unaltered
	// (the ICAO rendering when both exist). Always present. RawFormat says
	// whether it is NOTAM text or, as a last resort, the upstream record.
	RawText   string    `json:"raw_text"`
	RawFormat RawFormat `json:"raw_format"`
	// RawTextFormat names the rendering RawText holds (e.g. "ICAO",
	// "LOCAL_FORMAT"); empty when RawFormat is upstream_json.
	RawTextFormat string `json:"raw_text_format,omitempty"`
	// Translations are the other text renderings the source supplied, besides
	// the one in RawText, unaltered. RawText plus Translations is every
	// rendering, each exactly once. Empty when there is only one.
	Translations []Translation `json:"translations,omitempty"`

	// NMSID is the source's stable unique identifier; ID alone (the NOTAM
	// number) is not unique across locations.
	NMSID          string `json:"nms_id,omitempty"`
	Classification string `json:"classification,omitempty"` // DOM, INTL, FDC, MIL
	// Series is the ICAO NOTAM series letter, when stated.
	Series      string     `json:"series,omitempty"`
	IssuedAt    *time.Time `json:"issued_at,omitempty"`
	LastUpdated *time.Time `json:"last_updated,omitempty"`

	// ReturnedFor lists the location identifiers whose FAA query returned this
	// NOTAM, when the caller asked for several. A NOTAM returned for more than
	// one location appears once.
	ReturnedFor []string `json:"returned_for,omitempty"`

	// ParseIssues lists fields that could not be parsed. A record with parse
	// issues is still returned, never dropped.
	ParseIssues []string `json:"parse_issues,omitempty"`
}

// Translation is one text rendering of a NOTAM.
type Translation struct {
	Format string `json:"format"` // e.g. "ICAO", "LOCAL_FORMAT"
	Text   string `json:"text"`
}

// Others returns every rendering in all except the one at index primary (the
// one a record carries in RawText), or nil when there are none. Parsers use it
// so each rendering appears exactly once in a record.
func Others(all []Translation, primary int) []Translation {
	var out []Translation
	for i, t := range all {
		if i != primary {
			out = append(out, t)
		}
	}
	return out
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
	if n.RawFormat != RawOfficialText && n.RawFormat != RawUpstreamJSON {
		return fmt.Errorf("notam %s: invalid raw_format %q", n.ID, n.RawFormat)
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
	if n.Geometry != nil {
		if err := n.Geometry.Validate(); err != nil {
			return fmt.Errorf("notam %s: %w", n.ID, err)
		}
	}
	return nil
}
