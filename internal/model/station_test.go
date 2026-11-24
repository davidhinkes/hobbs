package model

import (
	"encoding/json"
	"testing"
	"time"
)

func TestStationReportValidate(t *testing.T) {
	pt := NewPoint(-117.86745, 33.67976)
	bad := NewPoint(-117.86745, 95)
	from := time.Date(2026, 11, 24, 6, 0, 0, 0, time.UTC)
	to := from.Add(24 * time.Hour)
	metar := func() StationReport {
		return StationReport{
			ID: "METAR KSNA 2026-11-24T05:53:00Z", Kind: KindMETAR, Source: "awc.metar",
			Station: "KSNA", RawText: "METAR KSNA 240553Z ...", RawFormat: RawMETAR, Geometry: &pt,
		}
	}
	taf := func() StationReport {
		return StationReport{
			ID: "TAF KSNA 2026-11-24T05:20:00Z", Kind: KindTAF, Source: "awc.taf",
			Station: "KSNA", RawText: "TAF KSNA 240520Z ...", RawFormat: RawTAF,
			ValidFrom: &from, ValidTo: &to,
		}
	}
	tests := []struct {
		name    string
		mod     func(*StationReport)
		base    func() StationReport
		wantErr bool
	}{
		{"valid metar", func(*StationReport) {}, metar, false},
		{"valid taf", func(*StationReport) {}, taf, false},
		{"missing id", func(r *StationReport) { r.ID = "" }, metar, true},
		{"missing kind", func(r *StationReport) { r.Kind = "" }, metar, true},
		{"missing source", func(r *StationReport) { r.Source = "" }, metar, true},
		{"missing station", func(r *StationReport) { r.Station = "" }, metar, true},
		{"metar with taf format", func(r *StationReport) { r.RawFormat = RawTAF }, metar, true},
		{"taf with metar format", func(r *StationReport) { r.RawFormat = RawMETAR }, taf, true},
		{"valid_to before valid_from", func(r *StationReport) { r.ValidFrom, r.ValidTo = &to, &from }, taf, true},
		{"no text and no record", func(r *StationReport) { r.RawText, r.RawFormat = "", "" }, metar, true},
		{"record without text", func(r *StationReport) {
			r.RawText, r.RawFormat = "", ""
			r.SourceRecord = json.RawMessage(`{"icaoId":"KSNA"}`)
		}, metar, false},
		{"bad geometry", func(r *StationReport) { r.Geometry = &bad }, metar, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := tc.base()
			tc.mod(&r)
			err := r.Validate()
			if (err != nil) != tc.wantErr {
				t.Fatalf("Validate() = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}
