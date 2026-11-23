package nms

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/davidhinkes/hobbs/internal/model"
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

// note collects problems found while parsing one record.
type note struct{ issues []string }

func (n *note) add(format string, a ...any) { n.issues = append(n.issues, fmt.Sprintf(format, a...)) }

func parseTime(s flexString, field string, n *note) *time.Time {
	if s == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339Nano, string(s))
	if err != nil {
		n.add("%s %q is not a timestamp", field, s)
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
			n.add("%s %q has no usable datum; falling back to Q-line levels", name, limit)
		}
		if qline != "" {
			if a, ok := parseQLineFL(string(qline), upper); ok {
				return a
			}
			n.add("Q-line %s level %q not understood", name, qline)
		}
		return model.Unknown()
	}
	return resolve(r.LowerLimit, r.MinimumFL, false, "lower limit"), resolve(r.UpperLimit, r.MaximumFL, true, "upper limit")
}

// parseCoord parses a Q-line position such as "3856N07727W".
func parseCoord(s string) (model.LatLon, bool) {
	m := coordRe.FindStringSubmatch(strings.ToUpper(strings.TrimSpace(s)))
	if m == nil {
		return model.LatLon{}, false
	}
	latD, _ := strconv.Atoi(m[1])
	latM, _ := strconv.Atoi(m[2])
	lonD, _ := strconv.Atoi(m[4])
	lonM, _ := strconv.Atoi(m[5])
	if latM >= 60 || lonM >= 60 || latD > 90 || lonD > 180 {
		return model.LatLon{}, false
	}
	lat := float64(latD) + float64(latM)/60
	lon := float64(lonD) + float64(lonM)/60
	if m[3] == "S" {
		lat = -lat
	}
	if m[6] == "W" {
		lon = -lon
	}
	return model.LatLon{Lat: lat, Lon: lon}, true
}

type geoJSON struct {
	Type       string          `json:"type"`
	Coords     json.RawMessage `json:"coordinates"`
	Geometries []geoJSON       `json:"geometries"`
}

func pos(c []float64) (model.LatLon, bool) {
	if len(c) < 2 {
		return model.LatLon{}, false
	}
	lon, lat := c[0], c[1]
	if math.IsNaN(lat) || math.IsNaN(lon) || lat < -90 || lat > 90 || lon < -180 || lon > 180 {
		return model.LatLon{}, false
	}
	return model.LatLon{Lat: lat, Lon: lon}, true
}

// geometry builds the spatial extent. All-or-nothing for areas: a partly
// usable multi-part area is dropped (and flagged) rather than understated.
// radiusNM, when known from the Q line, turns a point into a circle.
func geometry(raw json.RawMessage, radiusNM *float64, n *note) *model.Geometry {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil
	}
	var g geoJSON
	if err := json.Unmarshal(raw, &g); err != nil {
		n.add("geometry could not be decoded: %v", err)
		return nil
	}
	parts := g.Geometries
	if g.Type != "GeometryCollection" {
		parts = []geoJSON{g}
	}
	var points []model.LatLon
	var rings [][]model.LatLon
	for _, p := range parts {
		switch p.Type {
		case "Point":
			var c []float64
			if err := json.Unmarshal(p.Coords, &c); err != nil {
				n.add("point geometry could not be decoded")
				return nil
			}
			ll, ok := pos(c)
			if !ok {
				n.add("point geometry out of range")
				return nil
			}
			points = append(points, ll)
		case "Polygon", "MultiPolygon":
			var polys [][][][]float64
			if p.Type == "Polygon" {
				var poly [][][]float64
				if err := json.Unmarshal(p.Coords, &poly); err != nil {
					n.add("polygon geometry could not be decoded")
					return nil
				}
				polys = [][][][]float64{poly}
			} else if err := json.Unmarshal(p.Coords, &polys); err != nil {
				n.add("multipolygon geometry could not be decoded")
				return nil
			}
			for _, poly := range polys {
				if len(poly) == 0 {
					n.add("polygon has no rings")
					return nil
				}
				if len(poly) > 1 {
					n.add("polygon has %d hole(s) that are ignored, so the area is overstated", len(poly)-1)
				}
				ring := make([]model.LatLon, 0, len(poly[0]))
				for _, c := range poly[0] {
					ll, ok := pos(c)
					if !ok {
						n.add("polygon coordinate out of range")
						return nil
					}
					ring = append(ring, ll)
				}
				if len(ring) < 4 || ring[0] != ring[len(ring)-1] {
					n.add("polygon is not a closed ring of at least 4 points")
					return nil
				}
				rings = append(rings, ring)
			}
		default:
			n.add("unsupported geometry type %q", p.Type)
			return nil
		}
	}
	switch {
	case len(rings) > 0 && len(points) > 0:
		n.add("geometry mixes points and polygons; not used")
		return nil
	case len(rings) == 1:
		return &model.Geometry{Ring: rings[0]}
	case len(rings) > 1:
		return &model.Geometry{Rings: rings}
	case len(points) == 1:
		return &model.Geometry{Center: &points[0], RadiusNM: radiusNM}
	case len(points) > 1:
		n.add("%d points and no polygon; not used", len(points))
	}
	return nil
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
		return model.Notam{}, &note{issues: []string{"record could not be decoded: " + err.Error()}}
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
		n.add("unrecognized NOTAM type %q", t)
	}

	switch end := strings.TrimSpace(string(r.EffectiveEnd)); strings.ToUpper(end) {
	case "PERM":
		a.Permanent = true
	case "":
		n.add("effectiveEnd not stated")
	default:
		a.EffectiveTo = parseTime(flexString(end), "effectiveEnd", n)
	}
	if a.EffectiveFrom == nil && r.EffectiveStart == "" {
		n.add("effectiveStart not stated")
	}

	a.Lower, a.Upper = altitudes(r, n)

	var radius *float64
	if r.Radius != "" {
		if v, err := strconv.ParseFloat(string(r.Radius), 64); err == nil && v >= 0 {
			radius = &v
			a.RadiusNM = &v
		} else {
			n.add("radius %q not understood", r.Radius)
		}
	}
	if r.Coordinates != "" {
		if ll, ok := parseCoord(string(r.Coordinates)); ok {
			a.Center = &ll
		} else {
			n.add("Q-line coordinates %q not understood", r.Coordinates)
		}
	}
	a.Geometry = geometry(f.Geometry, radius, n)
	if a.Geometry == nil && a.Center != nil {
		// No usable GeoJSON geometry: fall back to the Q-line circle.
		a.Geometry = &model.Geometry{Center: a.Center, RadiusNM: radius}
	}

	// Raw text: ICAO rendering preferred, local otherwise; all kept.
	var icao, local string
	for _, t := range f.Properties.Core.Translation {
		text := string(t.FormattedText)
		if text == "" {
			text = string(t.SimpleText)
		}
		if text == "" {
			continue
		}
		a.Translations = append(a.Translations, model.Translation{Format: string(t.Type), Text: text})
		switch strings.ToUpper(string(t.Type)) {
		case "ICAO":
			icao = text
		case "LOCAL_FORMAT":
			local = text
		}
	}
	switch {
	case icao != "":
		a.RawText, a.RawFormat = icao, model.RawOfficialText
	case local != "":
		a.RawText, a.RawFormat = local, model.RawOfficialText
	case len(a.Translations) > 0:
		a.RawText, a.RawFormat = a.Translations[0].Text, model.RawOfficialText
	default:
		a.RawText, a.RawFormat = string(raw), model.RawUpstreamJSON
		n.add("no NOTAM text rendering supplied; upstream record kept verbatim")
	}
	return a, n
}
