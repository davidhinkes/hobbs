package nms

import (
	"bytes"
	"encoding/json"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/davidhinkes/hobbs/internal/geo"
	"github.com/davidhinkes/hobbs/internal/model"
	"github.com/davidhinkes/hobbs/internal/source"
)

// flexString decodes a JSON string, number or bool into text; null is empty.
type flexString string

func (s *flexString) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if bytes.Equal(b, []byte("null")) {
		return nil
	}
	if len(b) > 0 && b[0] == '"' {
		var v string
		if err := json.Unmarshal(b, &v); err != nil {
			return err
		}
		*s = flexString(v)
		return nil
	}
	*s = flexString(b)
	return nil
}

type rawNotam struct {
	ID             flexString `json:"id"`
	Number         flexString `json:"number"`
	Year           flexString `json:"year"`
	Type           flexString `json:"type"`
	Issued         flexString `json:"issued"`
	AffectedFIR    flexString `json:"affectedFir"`
	SelectionCode  flexString `json:"selectionCode"`
	MinimumFL      flexString `json:"minimumFl"`
	MaximumFL      flexString `json:"maximumFl"`
	Location       flexString `json:"location"`
	EffectiveStart flexString `json:"effectiveStart"`
	EffectiveEnd   flexString `json:"effectiveEnd"`
	Text           flexString `json:"text"`
	Classification flexString `json:"classification"`
	LastUpdated    flexString `json:"lastUpdated"`
	ICAOLocation   flexString `json:"icaoLocation"`
	Coordinates    flexString `json:"coordinates"`
	Radius         flexString `json:"radius"`
	Series         flexString `json:"series"`
	Estimated      flexString `json:"estimated"`
	LowerLimit     flexString `json:"lowerLimit"`
	UpperLimit     flexString `json:"upperLimit"`
	Schedule       flexString `json:"schedule"`
}

type rawFeature struct {
	Properties struct {
		Core struct {
			Notam       rawNotam `json:"notam"`
			Translation []struct {
				Type          flexString `json:"type"`
				SimpleText    flexString `json:"simpleText"`
				FormattedText flexString `json:"formattedText"`
			} `json:"notamTranslation"`
		} `json:"coreNOTAMData"`
	} `json:"properties"`
	Geometry json.RawMessage `json:"geometry"`
}

// note collects problems found while parsing one record (shared with the other adapters).
type note = source.Notes

func parseTime(s flexString, field string, n *note) *time.Time {
	if s == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339Nano, string(s))
	if err != nil {
		n.Add("%s %q is not a timestamp", field, s)
		return nil
	}
	t = t.UTC()
	return &t
}

var (
	limitFL   = regexp.MustCompile(`^FL(\d{1,3})$`)
	limitFeet = regexp.MustCompile(`^(\d+)\s*FT\s*(AMSL|AGL)$`)
	qlineFL   = regexp.MustCompile(`^\d{3}$`)
	coordRe   = regexp.MustCompile(`^(\d{2})(\d{2})([NS])(\d{3})(\d{2})([EW])$`)
)

// parseLimit parses a NOTAM item F/G limit: "SFC", "UNL", "FL200",
// "6000FT AMSL", "100FT AGL". A bare "135FT" has no datum and is not parsed.
func parseLimit(s string) (model.Altitude, bool) {
	v := strings.ToUpper(strings.TrimSpace(s))
	switch v {
	case "SFC", "GND":
		return model.SFC(), true
	case "UNL":
		return model.Unlimited(), true
	}
	if m := limitFL.FindStringSubmatch(v); m != nil {
		n, _ := strconv.Atoi(m[1])
		return model.FL(n), true
	}
	if m := limitFeet.FindStringSubmatch(v); m != nil {
		n, _ := strconv.Atoi(m[1])
		if m[2] == "AGL" {
			return model.AGL(n), true
		}
		return model.MSL(n), true
	}
	return model.Unknown(), false
}

// parseQLineFL parses a Q-line flight level (three digits). By ICAO
// convention 000 as a lower limit and 999 as an upper limit mean "no
// vertical limit stated", i.e. surface and unlimited.
func parseQLineFL(s string, upper bool) (model.Altitude, bool) {
	if !qlineFL.MatchString(s) {
		return model.Unknown(), false
	}
	n, _ := strconv.Atoi(s)
	switch {
	case !upper && n == 0:
		return model.SFC(), true
	case upper && n == 999:
		return model.Unlimited(), true
	}
	return model.FL(n), true
}

// altitudes resolves the vertical extent: the explicit limits (items F/G)
// when parseable, else the Q-line levels, else unknown. Anything that could
// not be understood is noted, and unknown is never turned into zero.
func altitudes(r rawNotam, n *note) (lower, upper model.Altitude) {
	resolve := func(limit, qline flexString, upper bool, name string) model.Altitude {
		if limit != "" {
			if a, ok := parseLimit(string(limit)); ok {
				return a
			}
			n.Add("%s %q has no usable datum; falling back to Q-line levels", name, limit)
		}
		if qline != "" {
			if a, ok := parseQLineFL(string(qline), upper); ok {
				return a
			}
			n.Add("Q-line %s level %q not understood", name, qline)
		}
		return model.Unknown()
	}
	return resolve(r.LowerLimit, r.MinimumFL, false, "lower limit"), resolve(r.UpperLimit, r.MaximumFL, true, "upper limit")
}

// parseCoord parses a Q-line position such as "3856N07727W" into a canonical
// [lon, lat] position.
func parseCoord(s string) (model.Position, bool) {
	m := coordRe.FindStringSubmatch(strings.ToUpper(strings.TrimSpace(s)))
	if m == nil {
		return model.Position{}, false
	}
	latD, _ := strconv.Atoi(m[1])
	latM, _ := strconv.Atoi(m[2])
	lonD, _ := strconv.Atoi(m[4])
	lonM, _ := strconv.Atoi(m[5])
	if latM >= 60 || lonM >= 60 || latD > 90 || lonD > 180 {
		return model.Position{}, false
	}
	lat := float64(latD) + float64(latM)/60
	lon := float64(lonD) + float64(lonM)/60
	if m[3] == "S" {
		lat = -lat
	}
	if m[6] == "W" {
		lon = -lon
	}
	return model.Position{lon, lat}, true
}

type geoJSON struct {
	Type       string          `json:"type"`
	Coords     json.RawMessage `json:"coordinates"`
	Geometries []geoJSON       `json:"geometries"`
}

func pos(c []float64) (model.Position, bool) {
	if len(c) < 2 {
		return model.Position{}, false
	}
	p := model.Position{c[0], c[1]} // GeoJSON order: lon, lat
	if math.IsNaN(p[0]) || math.IsNaN(p[1]) || p[0] < -180 || p[0] > 180 || p[1] < -90 || p[1] > 90 {
		return model.Position{}, false
	}
	return p, true
}

// noGeoJSON reports whether the record carries no GeoJSON geometry at all.
func noGeoJSON(raw json.RawMessage) bool {
	t := bytes.TrimSpace(raw)
	return len(t) == 0 || bytes.Equal(t, []byte("null"))
}

// noRadiusNM is the ICAO Q-line value meaning "no radius limit" (the NOTAM
// applies to a whole region): it is not a 999 nm circle.
const noRadiusNM = 999

// circleGeometry translates a Q-line circle (or a bare point when no radius is
// stated) into the canonical geometry, recording the circle as published.
func circleGeometry(center model.Position, radiusNM *float64, n *note) (*model.Geometry, *model.Derivation) {
	if radiusNM == nil {
		g := model.NewPoint(center[0], center[1])
		return &g, nil
	}
	if *radiusNM >= noRadiusNM {
		n.Add("radius %v means no radius limit (whole region); no geometry derived", *radiusNM)
		return nil, nil
	}
	g, steps, err := geo.Circle(center, *radiusNM)
	if err != nil {
		n.Add("circle could not be translated: %v", err)
		return nil, nil
	}
	d := &model.Derivation{Steps: steps}
	if g.Type != model.GeomPoint {
		d.Circle = &model.Circle{Center: center, RadiusNM: *radiusNM}
	}
	return &g, d
}

// geometry translates the feature's GeoJSON into the canonical geometry. All
// or nothing for areas: a partly usable multi-part area is dropped (and
// flagged) rather than understated. A point with a stated radius is a circle.
func geometry(raw json.RawMessage, radiusNM *float64, n *note) (*model.Geometry, *model.Derivation) {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil
	}
	var g geoJSON
	if err := json.Unmarshal(raw, &g); err != nil {
		n.Add("geometry could not be decoded: %v", err)
		return nil, nil
	}
	parts := g.Geometries
	if g.Type != "GeometryCollection" {
		parts = []geoJSON{g}
	}
	var points []model.Position
	var polys [][][]model.Position
	for _, p := range parts {
		switch p.Type {
		case "Point":
			var c []float64
			if err := json.Unmarshal(p.Coords, &c); err != nil {
				n.Add("point geometry could not be decoded")
				return nil, nil
			}
			ll, ok := pos(c)
			if !ok {
				n.Add("point geometry out of range")
				return nil, nil
			}
			points = append(points, ll)
		case "Polygon", "MultiPolygon":
			var in [][][][]float64
			if p.Type == "Polygon" {
				var poly [][][]float64
				if err := json.Unmarshal(p.Coords, &poly); err != nil {
					n.Add("polygon geometry could not be decoded")
					return nil, nil
				}
				in = [][][][]float64{poly}
			} else if err := json.Unmarshal(p.Coords, &in); err != nil {
				n.Add("multipolygon geometry could not be decoded")
				return nil, nil
			}
			for _, poly := range in {
				rings := make([][]model.Position, 0, len(poly))
				for _, r := range poly {
					ring := make([]model.Position, 0, len(r))
					for _, c := range r {
						ll, ok := pos(c)
						if !ok {
							n.Add("polygon coordinate out of range")
							return nil, nil
						}
						ring = append(ring, ll)
					}
					if len(ring) < 4 || ring[0] != ring[len(ring)-1] {
						n.Add("polygon is not a closed ring of at least 4 points")
						return nil, nil
					}
					rings = append(rings, ring)
				}
				if len(rings) == 0 {
					n.Add("polygon has no rings")
					return nil, nil
				}
				polys = append(polys, rings)
			}
		default:
			n.Add("unsupported geometry type %q", p.Type)
			return nil, nil
		}
	}
	switch {
	case len(polys) > 0 && len(points) > 0:
		n.Add("geometry mixes points and polygons; not used")
		return nil, nil
	case len(polys) > 0:
		g, steps, err := geo.Polygons(polys)
		if err != nil {
			n.Add("area could not be translated: %v", err)
			return nil, nil
		}
		if len(steps) == 0 {
			return &g, nil
		}
		return &g, &model.Derivation{Steps: steps}
	case len(points) == 1:
		return circleGeometry(points[0], radiusNM, n)
	case len(points) > 1:
		// A set of points with no area (e.g. the navaids of an airway restriction).
		// Passed through exactly as published, duplicates included.
		if radiusNM != nil {
			n.Add("a radius is stated for %d points; it is not applied", len(points))
		}
		g := model.NewMultiPoint(points)
		return &g, nil
	}
	return nil, nil
}

func truthy(s flexString) bool {
	switch strings.ToLower(strings.TrimSpace(string(s))) {
	case "true", "yes", "y", "1":
		return true
	}
	return false
}

// parseFeature converts one GeoJSON feature. It never fails outright for a
// decodable record: problems are collected in the note, and the record is
// kept with RawText and whatever could be parsed.
func parseFeature(raw json.RawMessage) (model.Notam, *note) {
	n := &note{}
	var f rawFeature
	if err := json.Unmarshal(raw, &f); err != nil {
		return model.Notam{}, &note{Issues: []string{"record could not be decoded: " + err.Error()}}
	}
	r := f.Properties.Core.Notam

	a := model.Notam{
		ID: string(r.Number), NMSID: string(r.ID), Classification: string(r.Classification),
		Series: string(r.Series), Location: string(r.ICAOLocation), FIR: string(r.AffectedFIR),
		QCode: string(r.SelectionCode), Text: string(r.Text), Schedule: string(r.Schedule),
		Estimated: truthy(r.Estimated),
		IssuedAt:  parseTime(r.Issued, "issued", n), LastUpdated: parseTime(r.LastUpdated, "lastUpdated", n),
		EffectiveFrom: parseTime(r.EffectiveStart, "effectiveStart", n),
	}
	if a.ID == "" {
		a.ID = a.NMSID
	}
	if a.Location == "" {
		a.Location = string(r.Location)
	}

	switch t := strings.ToUpper(string(r.Type)); t {
	case "N", "R", "C":
		a.Type = t
	case "":
		// Not stated by the source; left empty rather than guessed.
	default:
		n.Add("unrecognized NOTAM type %q", t)
	}

	switch end := strings.TrimSpace(string(r.EffectiveEnd)); strings.ToUpper(end) {
	case "PERM":
		a.Permanent = true
	case "":
		n.Add("effectiveEnd not stated")
	default:
		a.EffectiveTo = parseTime(flexString(end), "effectiveEnd", n)
	}
	if a.EffectiveFrom == nil && r.EffectiveStart == "" {
		n.Add("effectiveStart not stated")
	}

	a.Lower, a.Upper = altitudes(r, n)

	var radius *float64
	if r.Radius != "" {
		if v, err := strconv.ParseFloat(string(r.Radius), 64); err == nil && v >= 0 {
			radius = &v
		} else {
			n.Add("radius %q not understood", r.Radius)
		}
	}
	var qCenter *model.Position
	if r.Coordinates != "" {
		if ll, ok := parseCoord(string(r.Coordinates)); ok {
			qCenter = &ll
		} else {
			n.Add("Q-line coordinates %q not understood", r.Coordinates)
		}
	}
	a.Geometry, a.GeometryDerivation = geometry(f.Geometry, radius, n)
	if a.Geometry == nil && qCenter != nil && noGeoJSON(f.Geometry) {
		// The record carries no GeoJSON geometry at all: use its Q-line circle.
		a.Geometry, a.GeometryDerivation = circleGeometry(*qCenter, radius, n)
	}

	// Raw text: ICAO rendering preferred, local otherwise. The one chosen goes
	// in RawText (its format in RawTextFormat); Translations keeps every other
	// rendering, so each text appears once and none is lost.
	var all []model.Translation
	icao, local := -1, -1
	for _, t := range f.Properties.Core.Translation {
		text := string(t.FormattedText)
		if text == "" {
			text = string(t.SimpleText)
		}
		if text == "" {
			continue
		}
		switch strings.ToUpper(string(t.Type)) {
		case "ICAO":
			if icao < 0 {
				icao = len(all)
			}
		case "LOCAL_FORMAT":
			if local < 0 {
				local = len(all)
			}
		}
		all = append(all, model.Translation{Format: string(t.Type), Text: text})
	}
	primary := -1
	switch {
	case icao >= 0:
		primary = icao
	case local >= 0:
		primary = local
	case len(all) > 0:
		primary = 0
	}
	if primary >= 0 {
		a.RawText, a.RawTextFormat, a.RawFormat = all[primary].Text, all[primary].Format, model.RawOfficialText
		a.Translations = model.Others(all, primary)
	} else {
		a.RawText, a.RawFormat = string(raw), model.RawUpstreamJSON
		n.Add("no NOTAM text rendering supplied; upstream record kept verbatim")
	}
	return a, n
}
