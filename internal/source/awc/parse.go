package awc

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/davidhinkes/hobbs/internal/geo"
	"github.com/davidhinkes/hobbs/internal/model"
	"github.com/davidhinkes/hobbs/internal/source"
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

// note collects problems found while parsing one record (shared with the other adapters).
type note = source.Notes

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
// so the accepted input range is wider than a plain WGS84 longitude. They are
// translated into canonical RFC 7946 geometry by internal/geo.
const maxAbsLon = 360

// positions converts upstream points to canonical [lon, lat] positions,
// validating ranges.
func positions(cs []coord, n *note, what string) ([]model.Position, bool) {
	pts := make([]model.Position, 0, len(cs))
	for i, c := range cs {
		if c.Lat == nil || c.Lon == nil {
			n.Add("%s coordinate %d missing lat/lon", what, i)
			return nil, false
		}
		lat, lon := float64(*c.Lat), float64(*c.Lon)
		if lat < -90 || lat > 90 || lon < -maxAbsLon || lon > maxAbsLon {
			n.Add("%s coordinate %d out of range (%v, %v)", what, i, lat, lon)
			return nil, false
		}
		pts = append(pts, model.Position{lon, lat})
	}
	return pts, true
}

func isClosedRing(p []model.Position) bool { return len(p) >= 4 && p[0] == p[len(p)-1] }

// buildGeometry translates a feed's coordinates into the canonical geometry
// (Point/LineString/Polygon/MultiPolygon, RFC 7946). kind is the feed's stated
// type ("AREA", "AREAS", "LINE") or "" to infer. It returns nil and records a
// note when the coordinates cannot be used; coordinates are never auto-closed,
// and a multi-part area is all-or-nothing so an area is never understated.
// The derivation lists any translation applied (winding, antimeridian).
func buildGeometry(cs coordSet, kind string, n *note) (*model.Geometry, *model.Derivation) {
	if len(cs.rings) == 0 {
		n.Add("no coordinates")
		return nil, nil
	}
	rings := make([][]model.Position, 0, len(cs.rings))
	for i, r := range cs.rings {
		pts, ok := positions(r, n, fmt.Sprintf("part %d", i))
		if !ok {
			return nil, nil
		}
		rings = append(rings, pts)
	}
	areas := func() (*model.Geometry, *model.Derivation) {
		polys := make([][][]model.Position, 0, len(rings))
		for _, r := range rings {
			if !isClosedRing(r) {
				n.Add("area has a part that is not a closed ring of at least 4 points (%d points)", len(r))
				return nil, nil
			}
			polys = append(polys, [][]model.Position{r})
		}
		g, steps, err := geo.Polygons(polys)
		if err != nil {
			n.Add("area could not be translated: %v", err)
			return nil, nil
		}
		return &g, derivation(steps)
	}
	line := func(why string) (*model.Geometry, *model.Derivation) {
		if cs.nested || len(rings[0]) < 2 {
			n.Add("line must be a single list of at least 2 points")
			return nil, nil
		}
		for _, p := range rings[0] {
			if p[0] < -180 || p[0] > 180 {
				n.Add("line crosses the antimeridian; not represented")
				return nil, nil
			}
		}
		if why != "" {
			n.Add("%s", why)
		}
		g := model.NewLineString(rings[0])
		return &g, nil
	}

	switch k := strings.ToUpper(kind); k {
	case "LINE":
		return line("")
	case "AREA":
		if cs.nested {
			n.Add("geometry type AREA but coordinates are multi-part")
			return nil, nil
		}
		return areas()
	case "AREAS":
		if !cs.nested {
			n.Add("geometry type AREAS but coordinates are a single list")
			return nil, nil
		}
		return areas()
	case "":
		if cs.nested || isClosedRing(rings[0]) {
			return areas()
		}
		if len(rings[0]) >= 2 {
			// Open path with no stated type: report it as a line, not an area.
			return line("geometry type not stated and path is not closed; treated as a line")
		}
		n.Add("cannot determine geometry from %d point(s)", len(rings[0]))
		return nil, nil
	default:
		n.Add("unrecognized geometry type %q", kind)
		return nil, nil
	}
}

func derivation(steps []string) *model.Derivation {
	if len(steps) == 0 {
		return nil
	}
	return &model.Derivation{Steps: steps}
}

func orderWindow(a *model.Advisory, n *note) {
	if a.ValidFrom != nil && a.ValidTo != nil && a.ValidTo.Before(*a.ValidFrom) {
		n.Add("valid_to precedes valid_from; validity window discarded")
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
		return model.Advisory{}, &note{Issues: []string{"record could not be decoded: " + err.Error()}}
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
		RawText: r.Raw, RawFormat: model.RawAdvisoryText,
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
	a.Geometry, a.GeometryDerivation = buildGeometry(r.Coords, "", n)
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
		n.Add("%s %q not understood", field, v)
		return model.Unknown(), ""
	}
	return hundredsAlt(h), ""
}

func parseGAirmet(src string, raw json.RawMessage) (model.Advisory, *note) {
	n := &note{}
	var r gairmetRec
	if err := json.Unmarshal(raw, &r); err != nil {
		return model.Advisory{}, &note{Issues: []string{"record could not be decoded: " + err.Error()}}
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
		SourceRecord: model.SourceRecord(raw),
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
	a.Geometry, a.GeometryDerivation = buildGeometry(r.Coords, kind, n)
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
		return model.Advisory{}, &note{Issues: []string{"record could not be decoded: " + err.Error()}}
	}
	a := model.Advisory{
		ID: strings.TrimSpace(string(r.ICAOID) + " " + string(r.SeriesID)), Kind: model.KindIntlSIGMET, Source: src,
		Issuer: string(r.ICAOID), Hazard: string(r.Hazard), Qualifier: string(r.Qualifier),
		IssuedAt: tptr(r.ReceiptTime), ValidFrom: tptr(r.ValidFrom), ValidTo: tptr(r.ValidTo),
		Lower: model.Unknown(), Upper: model.Unknown(),
		RawText: r.Raw, RawFormat: model.RawAdvisoryText,
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
	a.Geometry, a.GeometryDerivation = buildGeometry(r.Coords, string(r.Geom), n)
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
		return model.Advisory{}, &note{Issues: []string{"record could not be decoded: " + err.Error()}}
	}
	a := model.Advisory{
		ID: strings.TrimSpace(string(r.CWSU) + " CWA " + string(r.SeriesID)), Kind: model.KindCWA, Source: src,
		Issuer: string(r.CWSU), Hazard: string(r.Hazard), Qualifier: string(r.Qualifier),
		IssuedAt: tptr(r.ReceiptTime), ValidFrom: tptr(r.ValidFrom), ValidTo: tptr(r.ValidTo),
		Lower: model.Unknown(), Upper: model.Unknown(),
		RawText: r.Raw, RawFormat: model.RawAdvisoryText,
	}
	detail(&a, "cwsu_name", &r.Name)
	if r.Top != nil {
		a.Upper = feetAlt(float64(*r.Top))
	}
	if r.Base != nil {
		a.Lower = feetAlt(float64(*r.Base))
	}
	a.Geometry, a.GeometryDerivation = buildGeometry(r.Coords, string(r.Geom), n)
	orderWindow(&a, n)
	return a, n
}
