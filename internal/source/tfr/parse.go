package tfr

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/davidhinkes/hobbs/internal/geo"
	"github.com/davidhinkes/hobbs/internal/model"
)

// ---- XML shapes (only the fields we use) ----

type xDoc struct {
	Not xNot `xml:"Group>Add>Not"`
}

type xNot struct {
	UID struct {
		LocalName  string `xml:"txtLocalName"`
		DateIssued string `xml:"dateIssued"`
	} `xml:"NotUid"`
	DateEffective  string `xml:"dateEffective"`
	DateExpire     string `xml:"dateExpire"`
	TimeZone       string `xml:"codeTimeZone"`
	ExpireTimeZone string `xml:"codeExpirationTimeZone"`
	Purpose        string `xml:"txtDescrPurpose"`
	Loc            struct {
		City  string `xml:"txtNameCity"`
		State string `xml:"txtNameUSState"`
	} `xml:"AffLocGroup"`
	Facility struct {
		Code  string `xml:",chardata"`
		Phone string `xml:"-"`
	} `xml:"codeFacility"`
	Tfr struct {
		Regulation string   `xml:"codeType"`
		Groups     []xGroup `xml:"TFRAreaGroup"`
	} `xml:"TfrNot"`
	CoordName  string   `xml:"txtNameCoordFacility"`
	CoordCode  string   `xml:"codeCoordFacility"`
	CoordPhone string   `xml:"txtAddrCoordPhone"`
	POCName    string   `xml:"txtNamePOC"`
	POCOrg     string   `xml:"txtNamePOCOrg"`
	POCPhone   []string `xml:"txtAddrPOCPhone"`
	POCFreq    string   `xml:"valFreqPOC"`
	USNS       string   `xml:"txtDescrUSNS"`
	Traditonal string   `xml:"txtDescrTraditional"`
}

type xGroup struct {
	Areas  []xArea   `xml:"aseTFRArea"`
	Merged []xMerged `xml:"abdMergedArea"`
}

type xArea struct {
	ID        string   `xml:"AseUid>codeId"`
	Name      string   `xml:"txtName"`
	UpperCode string   `xml:"codeDistVerUpper"`
	UpperVal  string   `xml:"valDistVerUpper"`
	UpperUOM  string   `xml:"uomDistVerUpper"`
	LowerCode string   `xml:"codeDistVerLower"`
	LowerVal  string   `xml:"valDistVerLower"`
	LowerUOM  string   `xml:"uomDistVerLower"`
	Scheduled string   `xml:"isScheduledTfrArea"`
	DayCodes  []string `xml:"dayCode"`
	Schedule  []struct {
		TimeSeparate  string `xml:"isTimeSeparate"`
		DateEffective string `xml:"dateEffective"`
		DateExpire    string `xml:"dateExpire"`
		StartTime     string `xml:"startTime"`
		EndTime       string `xml:"endTime"`
	} `xml:"ScheduleGroup"`
}

type xMerged struct {
	AreaID string `xml:"AbdUid>AseUid>codeId"`
	Avx    []struct {
		Datum string `xml:"codeDatum"`
		Type  string `xml:"codeType"`
		Lat   string `xml:"geoLat"`
		Lon   string `xml:"geoLong"`
	} `xml:"Avx"`
}

// note collects problems found while parsing one TFR.
type note struct{ issues []string }

func (n *note) add(format string, a ...any) { n.issues = append(n.issues, fmt.Sprintf(format, a...)) }

// maxRingSpanDeg: a published outline spanning more longitude than this is
// treated as an antimeridian crossing written with wrapped longitudes, which
// would read as a world-wide shape. TFR areas are local; none is anywhere near
// this wide, so such a ring is refused and flagged, never guessed.
const maxRingSpanDeg = 180

var hemi = regexp.MustCompile(`^(\d+(?:\.\d+)?)([NSEW])$`)

// parseHemi parses "39.35990574N" / "077.03638889W" into signed degrees.
func parseHemi(s string, wantLat bool) (float64, bool) {
	m := hemi.FindStringSubmatch(strings.ToUpper(strings.TrimSpace(s)))
	if m == nil {
		return 0, false
	}
	v, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0, false
	}
	switch m[2] {
	case "N", "S":
		if !wantLat || v > 90 {
			return 0, false
		}
	default:
		if wantLat || v > 180 {
			return 0, false
		}
	}
	if m[2] == "S" || m[2] == "W" {
		v = -v
	}
	return v, true
}

func clean(s string) string { return strings.TrimSpace(s) }

func cleanAll(ss []string) []string {
	var out []string
	for _, s := range ss {
		if c := clean(s); c != "" {
			out = append(out, c)
		}
	}
	return out
}

// singleRead lists, per parent element, the child elements this parser reads
// as single-valued. encoding/xml silently keeps only the LAST of a repeated
// single-valued element, which would drop data without a trace (it already did
// once, for dayCode). repeatedSingles scans the document for any such
// repetition so it is flagged on the record instead.
var singleRead = map[string]map[string]bool{
	"Not": {"dateEffective": true, "dateExpire": true, "codeTimeZone": true, "codeExpirationTimeZone": true,
		"txtDescrPurpose": true, "txtNameCoordFacility": true, "codeCoordFacility": true, "txtAddrCoordPhone": true,
		"txtNamePOC": true, "txtNamePOCOrg": true, "valFreqPOC": true, "txtDescrUSNS": true, "txtDescrTraditional": true,
		"NotUid": true, "AffLocGroup": true, "TfrNot": true},
	"TfrNot":      {"codeType": true},
	"NotUid":      {"txtLocalName": true, "dateIssued": true},
	"AffLocGroup": {"txtNameCity": true, "txtNameUSState": true},
	"aseTFRArea": {"AseUid": true, "txtName": true, "codeDistVerUpper": true, "valDistVerUpper": true, "uomDistVerUpper": true,
		"codeDistVerLower": true, "valDistVerLower": true, "uomDistVerLower": true, "isScheduledTfrArea": true},
	"abdMergedArea": {"AbdUid": true},
}

func repeatedSingles(raw []byte) []string {
	type frame struct {
		name   string
		counts map[string]int
	}
	var out []string
	dec := xml.NewDecoder(bytes.NewReader(bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf"))))
	var stack []frame
	for {
		tok, err := dec.Token()
		if err != nil {
			return out
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if len(stack) > 0 {
				p := &stack[len(stack)-1]
				if singleRead[p.name][t.Name.Local] {
					p.counts[t.Name.Local]++
				}
			}
			stack = append(stack, frame{t.Name.Local, map[string]int{}})
		case xml.EndElement:
			f := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			names := make([]string, 0, len(f.counts))
			for k, v := range f.counts {
				if v > 1 {
					names = append(names, fmt.Sprintf("%s repeats %d times under %s", k, v, f.name))
				}
			}
			sort.Strings(names)
			out = append(out, names...)
		}
	}
}

// altitude interprets one vertical limit. The published code and unit are kept
// by the caller. Mappings (all documented in docs/SPIKE.md):
//
//	ALT + FT  -> MSL feet          ALT + FL -> flight level
//	HEI + FT  -> AGL feet          (code absent) + FL -> flight level
//	value 999 in FL -> unlimited   anything else -> unknown, flagged
func altitude(code, val, uom, which string, n *note) model.Altitude {
	code, val, uom = strings.ToUpper(clean(code)), clean(val), strings.ToUpper(clean(uom))
	if val == "" {
		return model.Unknown()
	}
	f, err := strconv.ParseFloat(val, 64)
	if err != nil || f < 0 || math.IsNaN(f) || f != math.Trunc(f) {
		n.add("%s limit %q is not a whole number", which, val)
		return model.Unknown()
	}
	v := int(f)
	switch {
	case uom == "FL" && v == 999:
		return model.Unlimited()
	case uom == "FL" && (code == "ALT" || code == ""):
		return model.FL(v)
	case uom == "FT" && code == "ALT":
		return model.MSL(v)
	case uom == "FT" && code == "HEI":
		return model.AGL(v)
	}
	n.add("%s limit (%s %s %s) has no recognized datum; reported as unknown", which, code, val, uom)
	return model.Unknown()
}

// ring converts a merged outline to canonical positions, refusing anything
// we cannot translate faithfully.
func ring(m xMerged, n *note, areaID string) ([]model.Position, bool) {
	pts := make([]model.Position, 0, len(m.Avx))
	for i, v := range m.Avx {
		if t := strings.ToUpper(clean(v.Type)); t != "GRC" {
			n.add("area %s vertex %d has type %q, not a plain geodetic point", areaID, i, v.Type)
			return nil, false
		}
		if d := strings.ToUpper(clean(v.Datum)); d != "WGE" && d != "" {
			n.add("area %s vertex %d uses datum %q, not WGS84", areaID, i, v.Datum)
			return nil, false
		}
		lat, ok1 := parseHemi(v.Lat, true)
		lon, ok2 := parseHemi(v.Lon, false)
		if !ok1 || !ok2 {
			n.add("area %s vertex %d (%q, %q) is not a valid position", areaID, i, v.Lat, v.Lon)
			return nil, false
		}
		pts = append(pts, model.Position{lon, lat})
	}
	min, max := math.Inf(1), math.Inf(-1)
	for _, p := range pts {
		min, max = math.Min(min, p[0]), math.Max(max, p[0])
	}
	if len(pts) >= 4 && max-min > maxRingSpanDeg {
		n.add("area %s outline spans %.0f degrees of longitude; looks like an antimeridian crossing written with wrapped longitudes, not translated", areaID, max-min)
		return nil, false
	}
	if len(pts) < 4 || pts[0] != pts[len(pts)-1] {
		n.add("area %s outline is not a closed ring of at least 4 points (%d points)", areaID, len(pts))
		return nil, false
	}
	return pts, true
}

// parseDetail parses one detail document into t, which must already hold the
// list fields. It never fails outright for decodable XML: problems go in the
// note and the TFR is kept. A document that cannot be decoded at all returns
// an error and leaves t untouched.
func parseDetail(raw []byte, t *model.TFR, n *note) error {
	raw = bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf")) // the FAA serves a UTF-8 byte order mark
	var d xDoc
	dec := xml.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&d); err != nil {
		return fmt.Errorf("detail XML could not be decoded: %w", err)
	}
	x := d.Not
	if clean(x.USNS) == "" && clean(x.Traditonal) == "" && len(x.Tfr.Groups) == 0 && clean(x.UID.LocalName) == "" {
		return fmt.Errorf("detail XML has no NOTAM content")
	}

	t.DetailRetrieved = true
	for _, r := range repeatedSingles(raw) {
		n.add("%s; only the last value is read", r)
	}
	t.Purpose = clean(x.Purpose)
	t.Regulation = clean(x.Tfr.Regulation)
	t.IssuedRaw = clean(x.UID.DateIssued)
	t.EffectiveRaw, t.ExpireRaw = clean(x.DateEffective), clean(x.DateExpire)
	t.TimeZone, t.ExpireTimeZone = clean(x.TimeZone), clean(x.ExpireTimeZone)
	if city := clean(x.Loc.City); city != "" || clean(x.Loc.State) != "" {
		t.Location = strings.TrimSpace(city + ", " + clean(x.Loc.State))
		t.Location = strings.Trim(t.Location, ", ")
	}
	t.Effective = toUTC(t.EffectiveRaw, t.TimeZone, "effective", n)
	t.Expire = toUTC(t.ExpireRaw, t.ExpireTimeZone, "expire", n)

	det := map[string]string{}
	for k, v := range map[string]string{
		"coordination_facility": x.CoordName, "coordination_facility_code": x.CoordCode, "coordination_phone": x.CoordPhone,
		"poc_name": x.POCName, "poc_organization": x.POCOrg, "poc_phone": strings.Join(cleanAll(x.POCPhone), "; "), "poc_frequency": x.POCFreq,
	} {
		if clean(v) != "" {
			det[k] = clean(v)
		}
	}
	if len(det) > 0 {
		t.Details = det
	}

	// Text: the USNS rendering is the primary; every rendering is kept.
	for _, r := range []struct{ format, text string }{{"USNS", x.USNS}, {"TRADITIONAL", x.Traditonal}} {
		if clean(r.text) != "" {
			t.Translations = append(t.Translations, model.Translation{Format: r.format, Text: clean(r.text)})
		}
	}
	if len(t.Translations) > 0 {
		t.RawText, t.RawFormat = t.Translations[0].Text, model.RawOfficialText
	}

	// Areas: each altitude block joined to its published outline by area id.
	for _, g := range x.Tfr.Groups {
		outlines := map[string]xMerged{}
		for _, m := range g.Merged {
			outlines[clean(m.AreaID)] = m
		}
		used := map[string]bool{}
		for _, a := range g.Areas {
			area := model.TFRArea{
				ID: clean(a.ID), Name: clean(a.Name),
				Upper:     altitude(a.UpperCode, a.UpperVal, a.UpperUOM, "upper", n),
				Lower:     altitude(a.LowerCode, a.LowerVal, a.LowerUOM, "lower", n),
				Scheduled: strings.EqualFold(clean(a.Scheduled), "true"), DayCodes: cleanAll(a.DayCodes),
				Details: map[string]string{},
			}
			for k, v := range map[string]string{
				"upper_code": a.UpperCode, "upper_value": a.UpperVal, "upper_unit": a.UpperUOM,
				"lower_code": a.LowerCode, "lower_value": a.LowerVal, "lower_unit": a.LowerUOM,
			} {
				if clean(v) != "" {
					area.Details[k] = clean(v)
				}
			}
			for _, s := range a.Schedule {
				area.Schedules = append(area.Schedules, model.Schedule{
					DateEffective: clean(s.DateEffective), DateExpire: clean(s.DateExpire),
					StartTime: clean(s.StartTime), EndTime: clean(s.EndTime), TimeSeparate: clean(s.TimeSeparate),
				})
			}
			if m, ok := outlines[area.ID]; ok {
				used[area.ID] = true
				if pts, ok := ring(m, n, area.ID); ok {
					g, steps, err := geo.Polygons([][][]model.Position{{pts}})
					if err != nil {
						n.add("area %s outline could not be translated: %v", area.ID, err)
					} else {
						area.Geometry = &g
						if len(steps) > 0 {
							area.GeometryDerivation = &model.Derivation{Steps: steps}
						}
					}
				}
			}
			t.Areas = append(t.Areas, area)
		}
		for id := range outlines {
			if !used[id] {
				n.add("outline for area %q has no matching area; not used", id)
			}
		}
	}
	if len(x.Tfr.Groups) == 0 {
		n.add("no area groups published")
	}
	if t.RawText == "" {
		n.add("no NOTAM text rendering supplied")
	}
	return nil
}
