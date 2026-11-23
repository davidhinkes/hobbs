package tfr

import (
	"strings"
	"time"
)

// zoneOffsets maps the zone codes the FAA publishes to fixed UTC offsets. The
// codes are fixed-offset by construction (CDT is always UTC-5, CST always
// UTC-6), so no daylight-saving logic is involved. A code not listed here is
// not converted: the raw value stays and the gap is flagged.
var zoneOffsets = map[string]int{
	"UTC": 0, "GMT": 0, "Z": 0,
	"EST": -5, "EDT": -4, "CST": -6, "CDT": -5, "MST": -7, "MDT": -6, "PST": -8, "PDT": -7,
	"AKST": -9, "AKDT": -8, "HST": -10, "HAST": -10, "HADT": -9,
	"AST": -4, "ADT": -3, "SST": -11,
	"GUAM": 10, "CHST": 10,
}

const dateLayout = "2006-01-02T15:04:05"

// toUTC converts a published timestamp (no offset) in its stated zone code to
// UTC. It returns nil when the value is empty, malformed or in an unknown
// zone; the caller keeps the raw value, and a malformed or unknown case is noted.
func toUTC(raw, zone, field string, n *note) *time.Time {
	if raw == "" {
		return nil
	}
	off, ok := zoneOffsets[strings.ToUpper(strings.TrimSpace(zone))]
	if !ok {
		n.add("%s time zone %q is not recognized; time not converted to UTC", field, zone)
		return nil
	}
	t, err := time.Parse(dateLayout, raw)
	if err != nil {
		n.add("%s time %q is not a timestamp", field, raw)
		return nil
	}
	u := t.Add(-time.Duration(off) * time.Hour).UTC()
	return &u
}
