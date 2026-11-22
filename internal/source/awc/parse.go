package awc

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/davidhinkes/hobbs/internal/model"
)

// flightLevelFeet is the altitude at and above which AWC feet values are
// presented as flight levels. AWC publishes altitudes without a datum; this
// is the US transition altitude. It is an assumption and is reported to
// callers as ALTITUDE_ASSUMED on every snapshot.
const flightLevelFeet = 18000

type coord struct {
	Lat *flexFloat `json:"lat"`
	Lon *flexFloat `json:"lon"`
}

// note collects problems found while parsing one record.
type note struct{ issues []string }

func (n *note) add(format string, a ...any) { n.issues = append(n.issues, fmt.Sprintf(format, a...)) }

// ptr returns the time pointer or nil for an unset flexTime.
func tptr(t *flexTime) *time.Time {
	if t == nil || t.IsZero() {
		return nil
	}
	v := t.Time
	return &v
}

// feetAlt converts a feet value to an Altitude: flight level at or above
// flightLevelFeet, otherwise MSL feet.
func feetAlt(ft float64) model.Altitude {
	if ft >= flightLevelFeet {
		return model.FL(int(math.Round(ft / 100)))
	}
	return model.MSL(int(math.Round(ft)))
}

// hundredsAlt converts a value in hundreds of feet (G-AIRMET) to an Altitude.
func hundredsAlt(h float64) model.Altitude { return feetAlt(h * 100) }

// coordSet is a feed's "coords" value: either a flat list of points, or (for
// multi-part areas, geom "AREAS") a list of point lists.
type coordSet struct {
	rings  [][]coord
	nested bool
}

func (c *coordSet) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if bytes.Equal(b, []byte("null")) {
		return nil
	}
	var elems []json.RawMessage
	if err := json.Unmarshal(b, &elems); err != nil {
		return fmt.Errorf("coords is not a list: %w", err)
	}
	if len(elems) == 0 {
		return nil
	}
	switch first := bytes.TrimSpace(elems[0]); {
	case len(first) > 0 && first[0] == '{':
		var flat []coord
		if err := json.Unmarshal(b, &flat); err != nil {
			return err
		}
		c.rings = [][]coord{flat}
	case len(first) > 0 && first[0] == '[':
		if err := json.Unmarshal(b, &c.rings); err != nil {
			return err
		}
		c.nested = true
	default:
		return fmt.Errorf("unrecognized coords element %.20s", first)
	}
	return nil
}

// Longitudes beyond +/-180 occur in antimeridian-crossing features (e.g. 196.3),
// so the accepted range is wider than a plain WGS84 longitude.
const maxAbsLon = 360

func convert(cs []coord, n *note, what string) ([]model.LatLon, bool) {
	pts := make([]model.LatLon, 0, len(cs))
	for i, c := range cs {
		if c.Lat == nil || c.Lon == nil {
			n.add("%s coordinate %d missing lat/lon", what, i)
			return nil, false
		}
		lat, lon := float64(*c.Lat), float64(*c.Lon)
		if lat < -90 || lat > 90 || lon < -maxAbsLon || lon > maxAbsLon {
			n.add("%s coordinate %d out of range (%v, %v)", what, i, lat, lon)
			return nil, false
		}
		pts = append(pts, model.LatLon{Lat: lat, Lon: lon})
	}
	return pts, true
}

func isClosedRing(p []model.LatLon) bool { return len(p) >= 4 && p[0] == p[len(p)-1] }

// buildGeometry builds a geometry from a feed's coordinates. kind is the
// feed's stated type ("AREA", "AREAS", "LINE") or "" to infer. It returns nil
// and records a note when the coordinates cannot be used; coordinates are
// never altered or auto-closed, and a multi-part area is all-or-nothing so an
// area is never understated.
func buildGeometry(cs coordSet, kind string, n *note) *model.Geometry {
	if len(cs.rings) == 0 {
		n.add("no coordinates")
		return nil
	}
	rings := make([][]model.LatLon, 0, len(cs.rings))
	for i, r := range cs.rings {
		pts, ok := convert(r, n, fmt.Sprintf("part %d", i))
		if !ok {
			return nil
		}
		rings = append(rings, pts)
	}
	allClosed := func() bool {
		for _, r := range rings {
			if !isClosedRing(r) {
				return false
			}
		}
		return true
	}
	multi := func() *model.Geometry {
		if !allClosed() {
			n.add("multi-part area has a part that is not a closed ring of at least 4 points")
			return nil
		}
		if len(rings) == 1 {
			return &model.Geometry{Ring: rings[0]}
		}
		return &model.Geometry{Rings: rings}
	}

	switch k := strings.ToUpper(kind); k {
	case "LINE":
		if cs.nested || len(rings[0]) < 2 {
			n.add("line must be a single list of at least 2 points")
			return nil
		}
		return &model.Geometry{Line: rings[0]}
	case "AREA":
		if cs.nested {
			n.add("geometry type AREA but coordinates are multi-part")
			return nil
		}
		if !isClosedRing(rings[0]) {
			n.add("area is not a closed ring of at least 4 points (%d points)", len(rings[0]))
			return nil
		}
		return &model.Geometry{Ring: rings[0]}
	case "AREAS":
		if !cs.nested {
			n.add("geometry type AREAS but coordinates are a single list")
			return nil
		}
		return multi()
	case "":
		if cs.nested {
			return multi()
		}
		if isClosedRing(rings[0]) {
			return &model.Geometry{Ring: rings[0]}
		}
		if len(rings[0]) >= 2 {
			// Open path with no stated type: report it as a line, not an area.
			n.add("geometry type not stated and path is not closed; treated as a line")
			return &model.Geometry{Line: rings[0]}
		}
		n.add("cannot determine geometry from %d point(s)", len(rings[0]))
		return nil
	default:
		n.add("unrecognized geometry type %q", kind)
		return nil
	}
}

func orderWindow(a *model.Advisory, n *note) {
	if a.ValidFrom != nil && a.ValidTo != nil && a.ValidTo.Before(*a.ValidFrom) {
		n.add("valid_to precedes valid_from; validity window discarded")
		a.ValidFrom, a.ValidTo = nil, nil
	}
}

func detail(a *model.Advisory, key string, v *flexString) {
	if v == nil || *v == "" {
		return
	}
	if a.Details == nil {
		a.Details = map[string]string{}
	}
	a.Details[key] = string(*v)
}

// ---- airsigmet (US SIGMET / convective SIGMET; tolerates AIRMET) ----

type airSigmetRec struct {
	ICAOID       flexString  `json:"icaoId"`
	AlphaChar    flexString  `json:"alphaChar"`
	SeriesID     flexString  `json:"seriesId"`
	CreationTime *flexTime   `json:"creationTime"`
	ReceiptTime  *flexTime   `json:"receiptTime"`
	ValidFrom    *flexTime   `json:"validTimeFrom"`
	ValidTo      *flexTime   `json:"validTimeTo"`
	Type         flexString  `json:"airSigmetType"`
	Hazard       flexString  `json:"hazard"`
	Hi1          *flexFloat  `json:"altitudeHi1"`
	Hi2          *flexFloat  `json:"altitudeHi2"`
	Low1         *flexFloat  `json:"altitudeLow1"`
	Low2         *flexFloat  `json:"altitudeLow2"`
	MoveDir      *flexString `json:"movementDir"`
	MoveSpd      *flexString `json:"movementSpd"`
	Severity     *flexString `json:"severity"`
	Raw          string      `json:"rawAirSigmet"`
	Coords       coordSet    `json:"coords"`
}

// extent returns the widest vertical extent implied by two optional bounds:
// for a lower limit the minimum, for an upper limit the maximum, so that a
// sloping boundary is never understated.
func extent(a, b *flexFloat, wantMax bool) (float64, bool) {
	switch {
	case a == nil && b == nil:
		return 0, false
	case a == nil:
		return float64(*b), true
	case b == nil:
		return float64(*a), true
	}
	if wantMax {
		return math.Max(float64(*a), float64(*b)), true
	}
	return math.Min(float64(*a), float64(*b)), true
}

func parseAirSigmet(src string, raw json.RawMessage) (model.Advisory, *note) {
	n := &note{}
	var r airSigmetRec
	if err := json.Unmarshal(raw, &r); err != nil {
		return model.Advisory{}, &note{issues: []string{"record could not be decoded: " + err.Error()}}
	}
	kind := model.KindSIGMET
	if strings.EqualFold(string(r.Type), "AIRMET") {
		kind = model.KindAIRMET
	}
	a := model.Advisory{
		ID: strings.TrimSpace(string(r.ICAOID) + " " + string(r.SeriesID)), Kind: kind, Source: src,
		Issuer: string(r.ICAOID), Hazard: string(r.Hazard),
		IssuedAt: tptr(r.CreationTime), ValidFrom: tptr(r.ValidFrom), ValidTo: tptr(r.ValidTo),
		Lower: model.Unknown(), Upper: model.Unknown(),
		RawText: r.Raw, RawFormat: model.RawOfficialText,
	}
	if a.IssuedAt == nil {
		a.IssuedAt = tptr(r.ReceiptTime)
	}
	if r.Severity != nil {
		a.Severity = string(*r.Severity)
	}
	detail(&a, "movement_dir_deg", r.MoveDir)
	detail(&a, "movement_speed_kt", r.MoveSpd)
	if v, ok := extent(r.Low1, r.Low2, false); ok {
		a.Lower = feetAlt(v)
	}
	if v, ok := extent(r.Hi1, r.Hi2, true); ok {
		a.Upper = feetAlt(v)
	}
	a.Geometry = buildGeometry(r.Coords, "", n)
	orderWindow(&a, n)
	return a, n
}

// ---- gairmet (graphical AIRMET snapshots) ----

type gairmetRec struct {
	Tag          flexString  `json:"tag"`
	ForecastHour *flexFloat  `json:"forecastHour"`
	ValidTime    *flexTime   `json:"validTime"`
	Hazard       flexString  `json:"hazard"`
	GeomType     flexString  `json:"geometryType"`
	Geom         flexString  `json:"geom"`
	Severity     *flexString `json:"severity"`
	DueTo        *flexString `json:"due_to"`
	Status       *flexString `json:"status"`
	Top          *flexString `json:"top"`
	Base         *flexString `json:"base"`
	Level        *flexString `json:"level"`
	FZLTop       *flexString `json:"fzltop"`
	FZLBase      *flexString `json:"fzlbase"`
	Frequency    *flexString `json:"frequency"`
	IssueTime    *flexTime   `json:"issueTime"`
	ReceiptTime  *flexTime   `json:"receiptTime"`
	ExpireTime   *flexTime   `json:"expireTime"`
	Product      flexString  `json:"product"`
	Coords       coordSet    `json:"coords"`
}

// gairmetAlt parses a G-AIRMET vertical limit: hundreds of feet ("230"),
// "SFC", or "FZL" (freezing level). FZL has no fixed height, so it is left
// unknown and the stated reference is kept in Details by the caller.
func gairmetAlt(s *flexString, n *note, field string) (model.Altitude, string) {
	if s == nil || strings.TrimSpace(string(*s)) == "" {
		return model.Unknown(), ""
	}
	v := strings.ToUpper(strings.TrimSpace(string(*s)))
	switch v {
	case "SFC":
		return model.SFC(), ""
	case "FZL":
		return model.Unknown(), "FZL"
	}
	h, err := strconv.ParseFloat(v, 64)
	if err != nil || h < 0 {
		n.add("%s %q not understood", field, v)
		return model.Unknown(), ""
	}
	return hundredsAlt(h), ""
}

func parseGAirmet(src string, raw json.RawMessage) (model.Advisory, *note) {
	n := &note{}
	var r gairmetRec
	if err := json.Unmarshal(raw, &r); err != nil {
		return model.Advisory{}, &note{issues: []string{"record could not be decoded: " + err.Error()}}
	}
	id := fmt.Sprintf("G-AIRMET %s %s %s", r.Product, r.Tag, r.Hazard)
	if r.ForecastHour != nil {
		id += fmt.Sprintf(" +%dh", int(*r.ForecastHour))
	}
	a := model.Advisory{
		ID: strings.Join(strings.Fields(id), " "), Kind: model.KindAIRMET, Source: src,
		Issuer: "KKCI", Hazard: string(r.Hazard),
		IssuedAt: tptr(r.IssueTime), ValidFrom: tptr(r.ValidTime), ValidTo: tptr(r.ExpireTime),
		Lower: model.Unknown(), Upper: model.Unknown(),
		// G-AIRMET publishes no advisory text; keep the upstream record verbatim.
		RawText: string(raw), RawFormat: model.RawUpstreamJSON,
	}
	if a.IssuedAt == nil {
		a.IssuedAt = tptr(r.ReceiptTime)
	}
	if r.Severity != nil {
		a.Severity = string(*r.Severity)
	}
	if r.DueTo != nil {
		a.Qualifier = string(*r.DueTo)
	}
	var ref string
	a.Upper, _ = gairmetAlt(r.Top, n, "top")
	a.Lower, ref = gairmetAlt(r.Base, n, "base")
	if ref != "" {
		if a.Details == nil {
			a.Details = map[string]string{}
		}
		a.Details["base_reference"] = ref
	}
	detail(&a, "product", &r.Product)
	detail(&a, "status", r.Status)
	detail(&a, "level", r.Level)
	detail(&a, "freezing_level_top_hundreds_ft", r.FZLTop)
	detail(&a, "freezing_level_base_hundreds_ft", r.FZLBase)
	detail(&a, "frequency", r.Frequency)
	kind := string(r.GeomType)
	if kind == "" {
		kind = string(r.Geom)
	}
	a.Geometry = buildGeometry(r.Coords, kind, n)
	orderWindow(&a, n)
	return a, n
}

// ---- isigmet (international SIGMET) ----

type isigmetRec struct {
	ICAOID      flexString  `json:"icaoId"`
	FIRID       flexString  `json:"firId"`
	FIRName     flexString  `json:"firName"`
	ReceiptTime *flexTime   `json:"receiptTime"`
	ValidFrom   *flexTime   `json:"validTimeFrom"`
	ValidTo     *flexTime   `json:"validTimeTo"`
	SeriesID    flexString  `json:"seriesId"`
	Hazard      flexString  `json:"hazard"`
	Qualifier   flexString  `json:"qualifier"`
	Base        *flexFloat  `json:"base"`
	Top         *flexFloat  `json:"top"`
	Geom        flexString  `json:"geom"`
	Dir         *flexString `json:"dir"`
	Spd         *flexString `json:"spd"`
	Chng        *flexString `json:"chng"`
	Raw         string      `json:"rawSigmet"`
	Coords      coordSet    `json:"coords"`
}

func parseIntlSigmet(src string, raw json.RawMessage) (model.Advisory, *note) {
	n := &note{}
	var r isigmetRec
	if err := json.Unmarshal(raw, &r); err != nil {
		return model.Advisory{}, &note{issues: []string{"record could not be decoded: " + err.Error()}}
	}
	a := model.Advisory{
		ID: strings.TrimSpace(string(r.ICAOID) + " " + string(r.SeriesID)), Kind: model.KindIntlSIGMET, Source: src,
		Issuer: string(r.ICAOID), Hazard: string(r.Hazard), Qualifier: string(r.Qualifier),
		IssuedAt: tptr(r.ReceiptTime), ValidFrom: tptr(r.ValidFrom), ValidTo: tptr(r.ValidTo),
		Lower: model.Unknown(), Upper: model.Unknown(),
		RawText: r.Raw, RawFormat: model.RawOfficialText,
	}
	detail(&a, "fir_id", &r.FIRID)
	detail(&a, "fir_name", &r.FIRName)
	detail(&a, "movement_dir", r.Dir)
	detail(&a, "movement_speed_kt", r.Spd)
	detail(&a, "change", r.Chng)
	if r.Top != nil {
		a.Upper = feetAlt(float64(*r.Top))
	}
	if r.Base != nil {
		// AWC encodes a surface lower limit as 0: the official text of WAAA 14
		// reads "SFC/FL070" while the record has base 0. Zero feet MSL is never
		// a real lower limit here, so 0 is reported as SFC.
		if *r.Base == 0 {
			a.Lower = model.SFC()
		} else {
			a.Lower = feetAlt(float64(*r.Base))
		}
	}
	a.Geometry = buildGeometry(r.Coords, string(r.Geom), n)
	orderWindow(&a, n)
	return a, n
}

// ---- cwa (Center Weather Advisory) ----

type cwaRec struct {
	CWSU        flexString `json:"cwsu"`
	Name        flexString `json:"name"`
	ReceiptTime *flexTime  `json:"receiptTime"`
	ValidFrom   *flexTime  `json:"validTimeFrom"`
	ValidTo     *flexTime  `json:"validTimeTo"`
	SeriesID    flexString `json:"seriesId"`
	Hazard      flexString `json:"hazard"`
	Qualifier   flexString `json:"qualifier"`
	Base        *flexFloat `json:"base"`
	Top         *flexFloat `json:"top"`
	Geom        flexString `json:"geom"`
	Raw         string     `json:"rawText"`
	Coords      coordSet   `json:"coords"`
}

func parseCWA(src string, raw json.RawMessage) (model.Advisory, *note) {
	n := &note{}
	var r cwaRec
	if err := json.Unmarshal(raw, &r); err != nil {
		return model.Advisory{}, &note{issues: []string{"record could not be decoded: " + err.Error()}}
	}
	a := model.Advisory{
		ID: strings.TrimSpace(string(r.CWSU) + " CWA " + string(r.SeriesID)), Kind: model.KindCWA, Source: src,
		Issuer: string(r.CWSU), Hazard: string(r.Hazard), Qualifier: string(r.Qualifier),
		IssuedAt: tptr(r.ReceiptTime), ValidFrom: tptr(r.ValidFrom), ValidTo: tptr(r.ValidTo),
		Lower: model.Unknown(), Upper: model.Unknown(),
		RawText: r.Raw, RawFormat: model.RawOfficialText,
	}
	detail(&a, "cwsu_name", &r.Name)
	if r.Top != nil {
		a.Upper = feetAlt(float64(*r.Top))
	}
	if r.Base != nil {
		a.Lower = feetAlt(float64(*r.Base))
	}
	a.Geometry = buildGeometry(r.Coords, string(r.Geom), n)
	orderWindow(&a, n)
	return a, n
}
