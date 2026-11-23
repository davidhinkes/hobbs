package nms

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/davidhinkes/hobbs/internal/model"
	"github.com/davidhinkes/hobbs/internal/provenance"
	"github.com/davidhinkes/hobbs/internal/source"
)

const (
	// Authority names the service in provenance. Per the NMS terms of service,
	// redistributed data is not characterized as FAA data; the disclaimer says so.
	Authority = "NOTAM Management Service (NMS-API)"

	maxBody = 64 << 20
	// tokenSkew refreshes a token this long before it expires.
	tokenSkew = 60 * time.Second
	// MaxRadiusNM is the largest radius the API accepts.
	MaxRadiusNM = 100
)

var (
	// ErrUpstream wraps every failure to obtain usable NOTAM data.
	ErrUpstream = errors.New("nms upstream error")
	// ErrAuth is an authentication failure (bad credentials or token).
	ErrAuth = fmt.Errorf("%w: authentication failed", ErrUpstream)
	// ErrBadQuery is a query the API would reject; nothing was sent.
	ErrBadQuery = errors.New("invalid NOTAM query")
)

// Config configures a Client. ClientSecret is never logged or placed in errors.
type Config struct {
	// Host is the scheme and host only, e.g. https://api-staging.cgifederal-aim.com
	Host         string
	ClientID     string
	ClientSecret string
	HTTP         *http.Client
	Now          func() time.Time
}

// NMS-API hosts (scheme and host only), from the FAA's OpenAPI spec. Only
// staging has been verified; the production host is as the spec gives it.
const (
	StagingHost    = "https://api-staging.cgifederal-aim.com"
	ProductionHost = "https://api-nms.aim.faa.gov"
)

// CredentialsFromEnv reads HOBBS_NMS_CLIENT_ID and HOBBS_NMS_CLIENT_SECRET. Secrets come
// from the environment, never from command-line flags, so they do not appear
// in process listings or shell history. ok is false unless both are set.
func CredentialsFromEnv() (id, secret string, ok bool) {
	id, secret = os.Getenv("HOBBS_NMS_CLIENT_ID"), os.Getenv("HOBBS_NMS_CLIENT_SECRET")
	return id, secret, id != "" && secret != ""
}

// Client queries the NMS-API.
type Client struct {
	cfg  Config
	http *http.Client
	now  func() time.Time

	mu      sync.Mutex
	token   string
	expires time.Time
}

// NewClient validates the config and returns a Client.
func NewClient(cfg Config) (*Client, error) {
	u, err := url.Parse(cfg.Host)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, errors.New("nms: Host must be a URL with scheme and host")
	}
	if cfg.ClientID == "" || cfg.ClientSecret == "" {
		return nil, errors.New("nms: ClientID and ClientSecret are required")
	}
	c := &Client{cfg: cfg, http: cfg.HTTP, now: cfg.Now}
	c.cfg.Host = strings.TrimRight(cfg.Host, "/")
	if c.http == nil {
		c.http = &http.Client{Timeout: 60 * time.Second}
	}
	if c.now == nil {
		c.now = time.Now
	}
	return c, nil
}

// Query selects NOTAMs. Filters combine with AND, mirroring the API: it needs
// at least one filter, latitude/longitude/radius together, and effective start
// and end together.
type Query struct {
	Location       string // ICAO or domestic identifier
	Latitude       *float64
	Longitude      *float64
	RadiusNM       *float64 // 0..100
	EffectiveStart *time.Time
	EffectiveEnd   *time.Time
	LastUpdated    *time.Time // API limits this to the previous 24 hours
	Classification string     // one of Classifications (case-insensitive; the API wants the long names, e.g. DOMESTIC, while responses report DOM); with no other filter the API returns a download link, which this client does not support
	Accountability string
	Feature        string // one of Features (case-insensitive)
	FreeText       string
	NotamNumber    string // needs Location or Accountability
}

// Classifications and Features are the values the API accepts (from its
// OpenAPI spec, confirmed live for classification).
var (
	Classifications = []string{"DOMESTIC", "INTERNATIONAL", "MILITARY", "LOCAL_MILITARY", "FDC"}
	Features        = []string{"RWY", "TWY", "APRON", "AD", "OBST", "NAV", "COM", "SVC", "AIRSPACE", "ODP", "SID", "STAR",
		"CHART", "DATA", "DVA", "IAP", "VFP", "ROUTE", "SPECIAL", "SECURITY"}
)

func oneOf(v string, allowed []string) (string, bool) {
	for _, a := range allowed {
		if strings.EqualFold(v, a) {
			return a, true
		}
	}
	return "", false
}

func (q Query) validate() error {
	bad := func(f string, a ...any) error { return fmt.Errorf("%w: %s", ErrBadQuery, fmt.Sprintf(f, a...)) }
	if q.Classification != "" {
		if _, ok := oneOf(q.Classification, Classifications); !ok {
			return bad("classification %q is not one of %s", q.Classification, strings.Join(Classifications, ", "))
		}
	}
	if q.Feature != "" {
		if _, ok := oneOf(q.Feature, Features); !ok {
			return bad("feature %q is not one of %s", q.Feature, strings.Join(Features, ", "))
		}
	}
	geo := 0
	for _, set := range []bool{q.Latitude != nil, q.Longitude != nil, q.RadiusNM != nil} {
		if set {
			geo++
		}
	}
	if geo != 0 && geo != 3 {
		return bad("latitude, longitude and radius must be given together")
	}
	if q.Latitude != nil && (*q.Latitude < -90 || *q.Latitude > 90) {
		return bad("latitude %v out of range [-90, 90]", *q.Latitude)
	}
	if q.Longitude != nil && (*q.Longitude < -180 || *q.Longitude > 180) {
		return bad("longitude %v out of range [-180, 180]", *q.Longitude)
	}
	if q.RadiusNM != nil && (*q.RadiusNM < 0 || *q.RadiusNM > MaxRadiusNM) {
		return bad("radius %v out of range [0, %d] nm", *q.RadiusNM, MaxRadiusNM)
	}
	if (q.EffectiveStart == nil) != (q.EffectiveEnd == nil) {
		return bad("effective start and end must be given together")
	}
	if q.EffectiveStart != nil && q.EffectiveEnd.Before(*q.EffectiveStart) {
		return bad("effective end precedes start")
	}
	if q.NotamNumber != "" && q.Location == "" && q.Accountability == "" {
		return bad("notam number needs a location or accountability")
	}
	only := q.Classification != "" && geo == 0 && q.Location == "" && q.EffectiveStart == nil &&
		q.LastUpdated == nil && q.Accountability == "" && q.Feature == "" && q.FreeText == "" && q.NotamNumber == ""
	if only {
		return bad("classification alone returns a bulk-download link, which is not supported; add another filter")
	}
	if q.params().Encode() == "" {
		return bad("at least one filter is required")
	}
	return nil
}

func (q Query) params() url.Values {
	v := url.Values{}
	set := func(k, s string) {
		if s != "" {
			v.Set(k, s)
		}
	}
	f := func(x float64) string { return strconv.FormatFloat(x, 'f', -1, 64) }
	ts := func(t *time.Time) string {
		if t == nil {
			return ""
		}
		return t.UTC().Format("2006-01-02T15:04:05Z")
	}
	set("location", q.Location)
	if q.Latitude != nil {
		set("latitude", f(*q.Latitude))
	}
	if q.Longitude != nil {
		set("longitude", f(*q.Longitude))
	}
	if q.RadiusNM != nil {
		set("radius", f(*q.RadiusNM))
	}
	set("effectiveStartDate", ts(q.EffectiveStart))
	set("effectiveEndDate", ts(q.EffectiveEnd))
	set("lastUpdatedDate", ts(q.LastUpdated))
	if c, ok := oneOf(q.Classification, Classifications); ok {
		set("classification", c)
	}
	set("accountability", q.Accountability)
	if f, ok := oneOf(q.Feature, Features); ok {
		set("feature", f)
	}
	set("freeText", q.FreeText)
	set("notamNumber", q.NotamNumber)
	return v
}

// accessToken returns a valid bearer token, fetching a new one when needed.
func (c *Client) accessToken(ctx context.Context, force bool) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !force && c.token != "" && c.now().Add(tokenSkew).Before(c.expires) {
		return c.token, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.Host+"/v1/auth/token",
		strings.NewReader("grant_type=client_credentials"))
	if err != nil {
		return "", fmt.Errorf("%w: building token request", ErrUpstream)
	}
	req.SetBasicAuth(c.cfg.ClientID, c.cfg.ClientSecret)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.http.Do(req)
	if err != nil {
		// Do not include err: net/http errors can echo request details.
		return "", fmt.Errorf("%w: token request failed", ErrUpstream)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusForbidden {
			return "", fmt.Errorf("%w: token endpoint returned HTTP %d", ErrAuth, resp.StatusCode)
		}
		return "", fmt.Errorf("%w: token endpoint returned HTTP %d", ErrUpstream, resp.StatusCode)
	}
	var t struct {
		AccessToken string          `json:"access_token"`
		ExpiresIn   json.RawMessage `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &t); err != nil || t.AccessToken == "" {
		return "", fmt.Errorf("%w: token response not understood", ErrUpstream)
	}
	secs, _ := strconv.Atoi(strings.Trim(string(t.ExpiresIn), `" `))
	if secs <= 0 {
		secs = 300 // unstated lifetime: assume short rather than long
	}
	c.token = t.AccessToken
	c.expires = c.now().Add(time.Duration(secs) * time.Second)
	return c.token, nil
}

func (c *Client) invalidate() {
	c.mu.Lock()
	c.token, c.expires = "", time.Time{}
	c.mu.Unlock()
}

// Query fetches NOTAMs matching q. On failure it returns a Snapshot whose
// Source is filled in as far as the request got, with an error wrapping
// ErrUpstream (or ErrBadQuery if nothing was sent).
func (c *Client) Query(ctx context.Context, q Query) (source.Snapshot[model.Notam], error) {
	var snap source.Snapshot[model.Notam]
	if err := q.validate(); err != nil {
		return snap, err
	}
	endpoint := c.cfg.Host + "/nmsapi/v1/notams?" + q.params().Encode()
	snap.Source = provenance.Source{
		ID: SourceID, Authority: Authority, URL: endpoint,
		Documentation: provenance.Documented, ServedFrom: provenance.Live,
	}
	fail := func(format string, a ...any) (source.Snapshot[model.Notam], error) {
		return snap, fmt.Errorf("%w: %s", ErrUpstream, fmt.Sprintf(format, a...))
	}

	var resp *http.Response
	var body []byte
	for attempt := 0; attempt < 2; attempt++ {
		tok, err := c.accessToken(ctx, attempt > 0)
		if err != nil {
			snap.Source.RetrievedAt = c.now().UTC()
			return snap, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return fail("building request")
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("nmsResponseFormat", "GEOJSON")
		req.Header.Set("Accept", "application/json")
		resp, err = c.http.Do(req)
		snap.Source.RetrievedAt = c.now().UTC()
		if err != nil {
			return fail("request failed")
		}
		body, err = io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
		resp.Body.Close()
		if err != nil {
			return fail("reading body")
		}
		snap.Source.HTTPStatus = resp.StatusCode
		if resp.StatusCode == http.StatusUnauthorized && attempt == 0 {
			c.invalidate() // token may have expired early; retry once with a new one
			continue
		}
		break
	}
	snap.Source.ETag = resp.Header.Get("ETag")
	if len(body) > maxBody {
		return fail("response larger than %d bytes", maxBody)
	}
	sum := sha256.Sum256(body)
	snap.Source.ContentSHA256 = hex.EncodeToString(sum[:])

	if resp.StatusCode == http.StatusUnauthorized {
		return snap, ErrAuth
	}
	var envp struct {
		Status  string          `json:"status"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	}
	jsonErr := json.Unmarshal(body, &envp)
	if resp.StatusCode != http.StatusOK {
		msg := envp.Message
		if jsonErr != nil || msg == "" {
			msg = source.Truncate(string(body), 160)
		}
		return fail("HTTP %d: %s", resp.StatusCode, msg)
	}
	switch {
	case jsonErr != nil:
		return fail("body is not JSON")
	case envp.Status != "Success":
		return fail("status %q: %s", envp.Status, source.Truncate(envp.Message, 160))
	case len(envp.Data) == 0 || string(envp.Data) == "null":
		return fail("success response has no data field")
	}

	feats, err := features(envp.Data)
	if err != nil {
		return fail("%v", err)
	}
	c.build(&snap, feats)
	return snap, nil
}

// features extracts the feature list. An empty data object is the API's
// representation of "no matching NOTAMs".
func features(data json.RawMessage) ([]json.RawMessage, error) {
	var d struct {
		GeoJSON json.RawMessage `json:"geojson"`
	}
	if err := json.Unmarshal(data, &d); err != nil {
		return nil, errors.New("data is not an object")
	}
	if len(d.GeoJSON) == 0 || string(d.GeoJSON) == "null" {
		return nil, nil
	}
	var list []json.RawMessage
	if err := json.Unmarshal(d.GeoJSON, &list); err == nil {
		return list, nil
	}
	var fc struct {
		Features []json.RawMessage `json:"features"`
	}
	if err := json.Unmarshal(d.GeoJSON, &fc); err != nil {
		return nil, errors.New("geojson is neither a list nor a feature collection")
	}
	return fc.Features, nil
}

func (c *Client) build(snap *source.Snapshot[model.Notam], feats []json.RawMessage) {
	snap.Source.RecordsInFeed = len(feats)
	var parseFail, noGeom, noAlt, sched []string
	var newest time.Time
	for i, raw := range feats {
		a, n := parseFeature(raw)
		if a.ID == "" {
			// Undecodable or unidentifiable: keep a stub with the upstream JSON.
			if len(n.Issues) == 0 {
				n.Add("record has no identifier")
			}
			a = model.Notam{ID: fmt.Sprintf("%s#%d", SourceID, i), RawText: string(raw), RawFormat: model.RawUpstreamJSON,
				Lower: model.Unknown(), Upper: model.Unknown()}
		}
		a.ParseIssues = n.Issues
		if len(n.Issues) > 0 {
			parseFail = append(parseFail, a.ID)
		}
		if a.Geometry == nil {
			noGeom = append(noGeom, a.ID)
		}
		if !a.Lower.Known() && !a.Upper.Known() {
			noAlt = append(noAlt, a.ID)
		}
		if a.Schedule != "" {
			sched = append(sched, a.ID)
		}
		if a.LastUpdated != nil && a.LastUpdated.After(newest) {
			newest = *a.LastUpdated
		}
		if err := a.Validate(); err != nil {
			// A record that cannot satisfy the model is kept as a flagged stub
			// of the upstream JSON rather than dropped.
			a = model.Notam{ID: fmt.Sprintf("%s#%d", SourceID, i), RawText: string(raw), RawFormat: model.RawUpstreamJSON,
				Lower: model.Unknown(), Upper: model.Unknown(),
				ParseIssues: append(n.Issues, "record failed validation: "+err.Error())}
			parseFail = append(parseFail, a.ID)
		}
		snap.Items = append(snap.Items, a)
	}
	if !newest.IsZero() {
		snap.Source.UpstreamTimestamp = &newest
	}
	snap.Source.RecordsReturned = len(snap.Items)

	snap.Issues = source.AppendIssue(snap.Issues, SourceID, provenance.ParseFailed, provenance.Warning, parseFail,
		fmt.Sprintf("%d NOTAM(s) had fields that could not be parsed; they are included, flagged in parse_issues", len(parseFail)))
	snap.Issues = source.AppendIssue(snap.Issues, SourceID, provenance.GeometryUnavailable, provenance.Warning, noGeom,
		fmt.Sprintf("%d NOTAM(s) have no usable geometry as published", len(noGeom)))
	snap.Issues = source.AppendIssue(snap.Issues, SourceID, provenance.ScheduleUnevaluable, provenance.Warning, sched,
		fmt.Sprintf("%d NOTAM(s) carry a schedule that the server does not interpret; see the schedule field", len(sched)))
	snap.Issues = source.AppendIssue(snap.Issues, SourceID, provenance.AltitudeUnknown, provenance.Info, noAlt,
		fmt.Sprintf("%d NOTAM(s) state no vertical limits", len(noAlt)))
}

// Applied reports the filters this query forwards to the NMS-API, by the API's
// own parameter names. These are filters the source applies, not the server.
func (q Query) Applied() map[string]string {
	out := map[string]string{}
	for k, v := range q.params() {
		out[k] = v[0]
	}
	return out
}

// Host returns the API host this client talks to.
func (c *Client) Host() string { return c.cfg.Host }

// Ping checks that credentials work and the service is up: it obtains a token
// and calls the API's ping endpoint. It returns the HTTP status of the ping.
func (c *Client) Ping(ctx context.Context) (int, error) {
	for attempt := 0; attempt < 2; attempt++ {
		tok, err := c.accessToken(ctx, attempt > 0)
		if err != nil {
			return 0, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.cfg.Host+"/nmsapi/v1/ping", nil)
		if err != nil {
			return 0, fmt.Errorf("%w: building request", ErrUpstream)
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := c.http.Do(req)
		if err != nil {
			return 0, fmt.Errorf("%w: ping failed", ErrUpstream)
		}
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		resp.Body.Close()
		if resp.StatusCode == http.StatusUnauthorized && attempt == 0 {
			c.invalidate()
			continue
		}
		if resp.StatusCode == http.StatusUnauthorized {
			return resp.StatusCode, ErrAuth
		}
		if resp.StatusCode != http.StatusOK {
			return resp.StatusCode, fmt.Errorf("%w: ping returned HTTP %d", ErrUpstream, resp.StatusCode)
		}
		return resp.StatusCode, nil
	}
	return 0, ErrAuth
}
