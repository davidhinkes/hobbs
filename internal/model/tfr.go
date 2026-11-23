package model

import (
	"errors"
	"fmt"
	"time"
)

// TFR is a Temporary Flight Restriction as published by the FAA's TFR site:
// the list entry joined with its per-TFR detail document. Fields are as
// published; where we translated one (times to UTC, geometry to the canonical
// form) the raw value or a derivation is kept alongside.
type TFR struct {
	// ID is the NOTAM number as published, e.g. "6/9306".
	ID          string `json:"id"`
	Facility    string `json:"facility,omitempty"`
	State       string `json:"state,omitempty"`
	Type        string `json:"type,omitempty"` // HAZARDS, SECURITY, VIP, ... as published
	Description string `json:"description,omitempty"`
	GID         string `json:"gid,omitempty"`
	// LastModified is the list's mod_abs_time exactly as published; its time
	// zone is not stated, so it is not parsed.
	LastModified string `json:"last_modified,omitempty"`
	New          bool   `json:"new,omitempty"`

	// Effective and Expire are UTC, converted from the raw value in its stated
	// zone; nil when the source gives none or the zone is not recognized (the
	// raw values are always kept).
	Effective      *time.Time `json:"effective,omitempty"`
	Expire         *time.Time `json:"expire,omitempty"`
	EffectiveRaw   string     `json:"effective_raw,omitempty"`
	ExpireRaw      string     `json:"expire_raw,omitempty"`
	TimeZone       string     `json:"time_zone,omitempty"`
	ExpireTimeZone string     `json:"expire_time_zone,omitempty"`
	IssuedRaw      string     `json:"issued_raw,omitempty"`

	Purpose    string `json:"purpose,omitempty"`
	Regulation string `json:"regulation,omitempty"` // e.g. "91.137(a)(2)"
	Location   string `json:"location,omitempty"`

	// Areas are the TFR's altitude blocks, each with its own outline. A TFR
	// with no areas, or areas with no geometry, is text-only: its restriction is
	// described by RawText.
	Areas []TFRArea `json:"areas,omitempty"`

	// RawText is the NOTAM text as published (the "USNS" rendering when
	// present). RawFormat says what it holds. Translations lists every text
	// rendering supplied.
	RawText      string        `json:"raw_text"`
	RawFormat    RawFormat     `json:"raw_format"`
	Translations []Translation `json:"translations,omitempty"`

	// Details holds published contact and facility fields with no normalized home.
	Details map[string]string `json:"details,omitempty"`

	// DetailRetrieved is false when the detail document could not be fetched or
	// parsed and only the list entry is available.
	DetailRetrieved bool     `json:"detail_retrieved"`
	ParseIssues     []string `json:"parse_issues,omitempty"`
}

// TFRArea is one altitude block of a TFR with its outline.
type TFRArea struct {
	ID    string   `json:"id,omitempty"`
	Name  string   `json:"name,omitempty"`
	Lower Altitude `json:"lower"`
	Upper Altitude `json:"upper"`
	// Geometry is the FAA's own published outline of the area, in the canonical
	// form; nil when none is published.
	Geometry           *Geometry   `json:"geometry,omitempty"`
	GeometryDerivation *Derivation `json:"geometry_derivation,omitempty"`
	// Scheduled is true when the FAA marks the area as active on a schedule.
	// The schedule is passed through as published and is not interpreted.
	Scheduled bool `json:"scheduled,omitempty"`
	// DayCodes are the published day codes (the FAA repeats the element, e.g.
	// Tuesday, Thursday, Saturday), in published order.
	DayCodes  []string   `json:"day_codes,omitempty"`
	Schedules []Schedule `json:"schedules,omitempty"`
	// Details keeps the raw altitude fields exactly as published, so any
	// interpretation of Lower/Upper can be redone.
	Details map[string]string `json:"details,omitempty"`
}

// Schedule is one published schedule window, all values as published.
type Schedule struct {
	DateEffective string `json:"date_effective,omitempty"`
	DateExpire    string `json:"date_expire,omitempty"`
	StartTime     string `json:"start_time,omitempty"`
	EndTime       string `json:"end_time,omitempty"`
	TimeSeparate  string `json:"time_separate,omitempty"`
}

// Validate checks the invariants every TFR must satisfy before it is served.
func (t TFR) Validate() error {
	switch {
	case t.ID == "":
		return errors.New("tfr: id is required")
	case t.RawText == "":
		return fmt.Errorf("tfr %s: raw_text is required", t.ID)
	case t.RawFormat != RawOfficialText && t.RawFormat != RawUpstreamJSON:
		return fmt.Errorf("tfr %s: invalid raw_format %q", t.ID, t.RawFormat)
	case t.Effective != nil && t.Expire != nil && t.Expire.Before(*t.Effective):
		return fmt.Errorf("tfr %s: expire before effective", t.ID)
	}
	for i, a := range t.Areas {
		for _, alt := range []Altitude{a.Lower, a.Upper} {
			if err := alt.Validate(); err != nil {
				return fmt.Errorf("tfr %s area %d: %w", t.ID, i, err)
			}
		}
		if a.Geometry != nil {
			if err := a.Geometry.Validate(); err != nil {
				return fmt.Errorf("tfr %s area %d: %w", t.ID, i, err)
			}
		}
	}
	return nil
}
