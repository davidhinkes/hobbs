package model

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// ReportKind is the type of station report.
type ReportKind string

const (
	KindMETAR ReportKind = "METAR" // observation (METAR or SPECI)
	KindTAF   ReportKind = "TAF"   // terminal aerodrome forecast
)

// StationReport is a normalized METAR or TAF: a report about one station.
// AWC's decoded fields are kept as published in Details; hobbs computes
// nothing from them (no flight category, no ranking, no "latest" logic).
type StationReport struct {
	ID     string     `json:"id"`
	Kind   ReportKind `json:"kind"`
	Source string     `json:"source"` // provenance source id, e.g. "awc.metar"

	// ReportType is the source's own report type, e.g. "METAR" or "SPECI".
	ReportType  string   `json:"report_type,omitempty"`
	Station     string   `json:"station"` // ICAO identifier as published
	StationName string   `json:"station_name,omitempty"`
	ElevationM  *float64 `json:"elevation_m,omitempty"`
	// Geometry is the station position as a Point, when the source gave one.
	Geometry *Geometry `json:"geometry,omitempty"`

	// Times, each only when the source published it. ObservedAt, ReportedAt
	// and ReceivedAt belong to observations; IssuedAt, ValidFrom and ValidTo
	// to forecasts.
	ObservedAt *time.Time `json:"observed_at,omitempty"`
	ReportedAt *time.Time `json:"reported_at,omitempty"`
	ReceivedAt *time.Time `json:"received_at,omitempty"`
	IssuedAt   *time.Time `json:"issued_at,omitempty"`
	ValidFrom  *time.Time `json:"valid_from,omitempty"`
	ValidTo    *time.Time `json:"valid_to,omitempty"`

	// Details holds the source's decoded fields as published.
	Details json.RawMessage `json:"details,omitempty"`

	// RawText is the report as issued, unaltered; RawFormat is then metar or
	// taf to match Kind.
	RawText   string    `json:"raw_text,omitempty"`
	RawFormat RawFormat `json:"raw_format,omitempty"`
	// SourceRecord is the source's own record, verbatim, when there is no
	// published text or the record could not be parsed.
	SourceRecord json.RawMessage `json:"source_record,omitempty"`

	// ParseIssues lists fields that could not be parsed. Such records are
	// still returned, never dropped.
	ParseIssues []string `json:"parse_issues,omitempty"`
}

// Validate checks the invariants every StationReport must satisfy before it
// is served.
func (r StationReport) Validate() error {
	switch {
	case r.ID == "":
		return errors.New("station report: id is required")
	case r.Kind == "":
		return fmt.Errorf("station report %s: kind is required", r.ID)
	case r.Source == "":
		return fmt.Errorf("station report %s: source is required", r.ID)
	case r.Station == "":
		return fmt.Errorf("station report %s: station is required", r.ID)
	case r.ValidFrom != nil && r.ValidTo != nil && r.ValidTo.Before(*r.ValidFrom):
		return fmt.Errorf("station report %s: valid_to before valid_from", r.ID)
	}
	var want RawFormat
	switch r.Kind {
	case KindMETAR:
		want = RawMETAR
	case KindTAF:
		want = RawTAF
	default:
		return fmt.Errorf("station report %s: unknown kind %q", r.ID, r.Kind)
	}
	if err := validateText("station report", r.ID, r.RawText, r.RawFormat, []RawFormat{want}, r.SourceRecord); err != nil {
		return err
	}
	if r.Geometry != nil {
		if err := r.Geometry.Validate(); err != nil {
			return fmt.Errorf("station report %s: %w", r.ID, err)
		}
	}
	return nil
}
