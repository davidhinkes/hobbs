package model

import (
	"encoding/json"
	"fmt"
	"slices"
)

// RawFormat says what kind of published text a record's RawText holds. It is
// set exactly when RawText is. A record with no published text has neither;
// its SourceRecord holds what the source sent.
type RawFormat string

const (
	// RawICAONotam is NOTAM text in the ICAO format (items Q, A, B, C, E...).
	// The FAA NMS-API calls this rendering "ICAO".
	RawICAONotam RawFormat = "icao_notam"
	// RawUSNotam is NOTAM text in the US domestic format ("!EMT 02/017 ...").
	// The NMS-API calls it "LOCAL_FORMAT"; the TFR feed calls it "USNS".
	RawUSNotam RawFormat = "us_notam"
	// RawAdvisoryText is weather advisory text as issued (SIGMET, CWA, ...).
	RawAdvisoryText RawFormat = "advisory_text"
	// RawMETAR is a METAR or SPECI report as issued ("METAR KSNA 240553Z ...").
	RawMETAR RawFormat = "metar"
	// RawTAF is a terminal aerodrome forecast as issued ("TAF KSNA 240520Z ...").
	RawTAF RawFormat = "taf"
)

// SourceRecord returns raw as a value for a SourceRecord field: raw itself
// when it is valid JSON, else raw as a JSON string, so that even a record that
// is not valid JSON is kept rather than dropped.
func SourceRecord(raw []byte) json.RawMessage {
	if json.Valid(raw) {
		return json.RawMessage(slices.Clone(raw))
	}
	s, _ := json.Marshal(string(raw))
	return s
}

// validateText checks the text invariants shared by every record type: a
// record carries published text (RawText, with a RawFormat from allowed) or,
// failing that, the source's own record (SourceRecord), or both; RawFormat is
// set exactly when RawText is.
func validateText(what, id, text string, f RawFormat, allowed []RawFormat, rec json.RawMessage) error {
	switch {
	case text == "" && f != "":
		return fmt.Errorf("%s %s: raw_format %q without raw_text", what, id, f)
	case text == "" && len(rec) == 0:
		return fmt.Errorf("%s %s: raw_text or source_record is required", what, id)
	case text != "" && !slices.Contains(allowed, f):
		return fmt.Errorf("%s %s: invalid raw_format %q", what, id, f)
	case len(rec) > 0 && !json.Valid(rec):
		return fmt.Errorf("%s %s: source_record is not valid JSON", what, id)
	}
	return nil
}
