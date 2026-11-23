// Package awc adapts the NOAA Aviation Weather Center Data API
// (aviationweather.gov/api/data): SIGMET, G-AIRMET, international SIGMET and
// Center Weather Advisories.
//
// Behavior here is based on observed responses (docs/SPIKE.md):
//   - "no results" is HTTP 204 with an empty body; 400 is a bad parameter.
//   - upstream filters are unreliable, so whole feeds are fetched and
//     filtering is left to the caller.
//   - types and time formats vary by feed (see flex.go).
package awc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/davidhinkes/hobbs/internal/model"
	"github.com/davidhinkes/hobbs/internal/provenance"
	"github.com/davidhinkes/hobbs/internal/source"
)

const (
	// Authority names the issuing organization in provenance.
	Authority = "NOAA/NWS Aviation Weather Center"
	// DefaultBaseURL is the AWC Data API root.
	DefaultBaseURL = "https://aviationweather.gov/api/data"
	// maxBody caps a response so a misbehaving upstream cannot exhaust memory.
	maxBody = 32 << 20
)

// Source IDs used in provenance.
const (
	IDAirSigmet  = "awc.airsigmet"
	IDGAirmet    = "awc.gairmet"
	IDIntlSigmet = "awc.isigmet"
	IDCWA        = "awc.cwa"
)

// ErrUpstream wraps every failure to obtain usable data from AWC.
var ErrUpstream = errors.New("awc upstream error")

// Client fetches AWC feeds.
type Client struct {
	HTTP      *http.Client
	BaseURL   string
	UserAgent string
	// Now is the clock; injectable for tests.
	Now func() time.Time
}

// New returns a Client with sane defaults.
func New() *Client {
	return &Client{
		HTTP:      &http.Client{Timeout: 30 * time.Second},
		BaseURL:   DefaultBaseURL,
		UserAgent: source.UserAgent,
		Now:       time.Now,
	}
}

type parser func(src string, raw json.RawMessage) (model.Advisory, *note)

type feed struct {
	c     *Client
	id    string
	path  string
	kind  model.AdvisoryKind
	parse parser
}

var _ source.Source[model.Advisory] = (*feed)(nil)

func (c *Client) AirSigmet() source.Source[model.Advisory] {
	return &feed{c, IDAirSigmet, "airsigmet", model.KindSIGMET, parseAirSigmet}
}
func (c *Client) GAirmet() source.Source[model.Advisory] {
	return &feed{c, IDGAirmet, "gairmet", model.KindAIRMET, parseGAirmet}
}
func (c *Client) IntlSigmet() source.Source[model.Advisory] {
	return &feed{c, IDIntlSigmet, "isigmet", model.KindIntlSIGMET, parseIntlSigmet}
}
func (c *Client) CWA() source.Source[model.Advisory] {
	return &feed{c, IDCWA, "cwa", model.KindCWA, parseCWA}
}

func (f *feed) ID() string { return f.id }

// Fetch retrieves the whole feed. On failure it returns a Snapshot whose
// Source is filled in as far as the fetch got (URL, time, HTTP status) along
// with an error wrapping ErrUpstream, so callers can still attribute the
// failure.
func (f *feed) Fetch(ctx context.Context) (source.Snapshot[model.Advisory], error) {
	url := f.c.BaseURL + "/" + f.path + "?format=json"
	snap := source.Snapshot[model.Advisory]{Source: provenance.Source{
		ID: f.id, Authority: Authority, URL: url,
		Documentation: provenance.Documented, ServedFrom: provenance.Live,
	}}
	fail := func(format string, a ...any) (source.Snapshot[model.Advisory], error) {
		return snap, fmt.Errorf("%w: %s: %s", ErrUpstream, f.id, fmt.Sprintf(format, a...))
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fail("building request: %v", err)
	}
	req.Header.Set("User-Agent", f.c.UserAgent)
	req.Header.Set("Accept", "application/json")

	resp, err := f.c.HTTP.Do(req)
	snap.Source.RetrievedAt = f.c.Now().UTC()
	if err != nil {
		return fail("request failed: %v", err)
	}
	defer resp.Body.Close()
	snap.Source.HTTPStatus = resp.StatusCode
	snap.Source.ETag = resp.Header.Get("ETag")

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return fail("reading body: %v", err)
	}
	if len(body) > maxBody {
		return fail("response larger than %d bytes", maxBody)
	}
	sum := sha256.Sum256(body)
	snap.Source.ContentSHA256 = hex.EncodeToString(sum[:])

	switch {
	case resp.StatusCode == http.StatusNoContent && len(body) == 0:
		// AWC's documented-by-observation "no matching records". A genuine
		// empty feed, not a failure.
		return snap, nil
	case resp.StatusCode != http.StatusOK:
		return fail("HTTP %d: %s", resp.StatusCode, source.Truncate(string(body), 200))
	case len(strings.TrimSpace(string(body))) == 0:
		// 200 with no body is not how AWC reports "no results" (that is 204),
		// so do not treat it as an empty feed.
		return fail("HTTP 200 with an empty body")
	}

	var recs []json.RawMessage
	if err := json.Unmarshal(body, &recs); err != nil {
		return fail("body is not a JSON array: %v", err)
	}
	snap.Source.RecordsInFeed = len(recs)

	var (
		parseFail, noGeom, lines, noAlt []string
		newest                          time.Time
		seen                            = map[string]int{}
	)
	for i, raw := range recs {
		a, n := f.parse(f.id, raw)
		if a.ID == "" || a.Kind == "" {
			// Undecodable or unidentifiable record: keep it as a stub with the
			// upstream JSON, never drop it.
			if len(n.Issues) == 0 {
				n.Add("record has no identifier")
			}
			a = model.Advisory{
				ID: fmt.Sprintf("%s#%d", f.id, i), Kind: f.kind, Source: f.id,
				Lower: model.Unknown(), Upper: model.Unknown(),
				RawText: string(raw), RawFormat: model.RawUpstreamJSON,
			}
		}
		if a.RawText == "" {
			a.RawText, a.RawFormat = string(raw), model.RawUpstreamJSON
			n.Add("official text missing; upstream record kept verbatim")
		}
		if c := seen[a.ID]; c > 0 {
			seen[a.ID]++
			a.ID = fmt.Sprintf("%s #%d", a.ID, c+1)
		} else {
			seen[a.ID] = 1
		}
		a.ParseIssues = n.Issues
		if len(n.Issues) > 0 {
			parseFail = append(parseFail, a.ID)
		}
		switch {
		case a.Geometry == nil:
			noGeom = append(noGeom, a.ID)
		case len(a.Geometry.Line) > 0:
			lines = append(lines, a.ID)
		}
		if !a.Lower.Known() && !a.Upper.Known() {
			noAlt = append(noAlt, a.ID)
		}
		if a.IssuedAt != nil && a.IssuedAt.After(newest) {
			newest = *a.IssuedAt
		}
		if err := a.Validate(); err != nil {
			return fail("internal: record %d failed validation: %v", i, err)
		}
		snap.Items = append(snap.Items, a)
	}
	if !newest.IsZero() {
		snap.Source.UpstreamTimestamp = &newest
	}
	snap.Source.RecordsReturned = len(snap.Items)

	snap.Issues = source.AppendIssue(snap.Issues, f.id, provenance.ParseFailed, provenance.Warning, parseFail,
		fmt.Sprintf("%d record(s) had fields that could not be parsed; they are included, flagged in parse_issues", len(parseFail)))
	snap.Issues = source.AppendIssue(snap.Issues, f.id, provenance.GeometryUnavailable, provenance.Warning, noGeom,
		fmt.Sprintf("%d record(s) have no usable geometry as published", len(noGeom)))
	snap.Issues = source.AppendIssue(snap.Issues, f.id, provenance.GeometryNotArea, provenance.Info, lines,
		fmt.Sprintf("%d record(s) are lines, not areas (e.g. freezing-level contours)", len(lines)))
	snap.Issues = source.AppendIssue(snap.Issues, f.id, provenance.AltitudeUnknown, provenance.Info, noAlt,
		fmt.Sprintf("%d record(s) state no vertical limits", len(noAlt)))
	snap.Issues = append(snap.Issues, provenance.Issue{
		Code: provenance.AltitudeAssumed, Severity: provenance.Info, Source: f.id,
		Message: "AWC states altitudes without a datum; interpreted as feet, shown as flight levels at or above 18,000 ft",
	})
	return snap, nil
}
