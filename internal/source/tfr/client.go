// Package tfr adapts the FAA's TFR site (tfr.faa.gov): the list of TFRs and,
// for each, its detail document (XML) holding dates, altitudes, outline and
// the NOTAM text.
//
// These endpoints are not a documented, supported API (docs/SPIKE.md), so every
// snapshot is marked undocumented by the provenance contract, and behavior
// here follows observed responses.
//
// The adapter is OFF by default: a Client refuses to fetch (ErrDisabled, with
// no network request) unless Enabled is set. Nothing in the server sets it.
// It is kept in the code base, built and tested, until there is a source we
// can use.
package tfr

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
	"sync"
	"time"

	"github.com/davidhinkes/hobbs/internal/model"
	"github.com/davidhinkes/hobbs/internal/provenance"
	"github.com/davidhinkes/hobbs/internal/source"
)

const (
	// SourceID identifies this source in provenance.
	SourceID = "faa.tfr"
	// Authority names the issuer in provenance.
	Authority = "FAA TFR site (tfr.faa.gov)"
	// DefaultBaseURL is the TFR site root.
	DefaultBaseURL = "https://tfr.faa.gov"

	maxList   = 8 << 20
	maxDetail = 4 << 20
	// concurrency bounds simultaneous detail requests.
	concurrency = 8
)

var (
	// ErrUpstream wraps a failure to obtain the TFR list (no usable data at all).
	ErrUpstream = errors.New("tfr upstream error")
	// ErrDisabled is returned by a Client that has not been enabled.
	ErrDisabled = errors.New("tfr adapter is disabled")
)

// Client fetches TFRs.
type Client struct {
	// Enabled must be set explicitly for the client to make any request.
	Enabled   bool
	HTTP      *http.Client
	BaseURL   string
	UserAgent string
	Now       func() time.Time
}

// New returns a Client with sane defaults.
func New() *Client {
	return &Client{
		HTTP:      &http.Client{Timeout: 30 * time.Second},
		BaseURL:   DefaultBaseURL,
		UserAgent: "hobbs-mcp (aviation advisories data layer)",
		Now:       time.Now,
	}
}

// Filter narrows the list by exact (case-insensitive) match on fields the FAA
// itself publishes in the list. It is applied by this server, not by the FAA,
// and only before detail documents are fetched; an empty Filter returns every
// TFR. IDs matches the published NOTAM number.
type Filter struct {
	State    string
	Facility string
	Type     string
	IDs      []string
}

func (f Filter) match(e listEntry) bool {
	eq := func(want, got string) bool {
		return want == "" || strings.EqualFold(strings.TrimSpace(want), strings.TrimSpace(got))
	}
	if !eq(f.State, e.State) || !eq(f.Facility, e.Facility) || !eq(f.Type, e.Type) {
		return false
	}
	if len(f.IDs) == 0 {
		return true
	}
	for _, id := range f.IDs {
		if strings.EqualFold(strings.TrimSpace(id), strings.TrimSpace(e.NotamID)) {
			return true
		}
	}
	return false
}

type listEntry struct {
	NotamID     string `json:"notam_id"`
	Facility    string `json:"facility"`
	State       string `json:"state"`
	Type        string `json:"type"`
	Description string `json:"description"`
	ModDate     string `json:"mod_date"`
	ModAbsTime  string `json:"mod_abs_time"`
	IsNew       string `json:"is_new"`
	GID         string `json:"gid"`
}

// Source adapts the client to the shared interface with no filter.
func (c *Client) Source() source.Source[model.TFR] { return unfiltered{c} }

type unfiltered struct{ c *Client }

func (u unfiltered) ID() string { return SourceID }
func (u unfiltered) Fetch(ctx context.Context) (source.Snapshot[model.TFR], error) {
	return u.c.Fetch(ctx, Filter{})
}

func detailPath(id string) string {
	return "/download/detail_" + strings.ReplaceAll(strings.TrimSpace(id), "/", "_") + ".xml"
}

func (c *Client) get(ctx context.Context, url string, limit int64) (body []byte, status int, hdr http.Header, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, nil, err
	}
	req.Header.Set("User-Agent", c.UserAgent)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, 0, nil, err
	}
	defer resp.Body.Close()
	body, err = io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, resp.StatusCode, resp.Header, err
	}
	if int64(len(body)) > limit {
		return nil, resp.StatusCode, resp.Header, fmt.Errorf("response larger than %d bytes", limit)
	}
	return body, resp.StatusCode, resp.Header, nil
}

// Fetch reads the TFR list and, for every entry that passes the filter, its
// detail document. It returns a Snapshot with every listed TFR that passed the
// filter: a TFR whose detail could not be fetched or parsed is still returned
// with its list fields and a warning, never dropped. It returns an error (and
// a Snapshot that still attributes the attempt) only when the list itself
// cannot be obtained.
func (c *Client) Fetch(ctx context.Context, f Filter) (source.Snapshot[model.TFR], error) {
	if !c.Enabled {
		return source.Snapshot[model.TFR]{}, ErrDisabled
	}
	listURL := c.BaseURL + "/tfrapi/getTfrList"
	snap := source.Snapshot[model.TFR]{Source: provenance.Source{
		ID: SourceID, Authority: Authority, URL: listURL,
		Documentation: provenance.Undocumented, ServedFrom: provenance.Live,
	}}
	fail := func(format string, a ...any) (source.Snapshot[model.TFR], error) {
		return snap, fmt.Errorf("%w: %s", ErrUpstream, fmt.Sprintf(format, a...))
	}

	body, status, hdr, err := c.get(ctx, listURL, maxList)
	snap.Source.RetrievedAt = c.Now().UTC()
	if err != nil {
		return fail("list request failed: %v", err)
	}
	snap.Source.HTTPStatus = status
	snap.Source.ETag = hdr.Get("ETag")
	sum := sha256.Sum256(body)
	snap.Source.ContentSHA256 = hex.EncodeToString(sum[:])
	if status != http.StatusOK {
		return fail("list returned HTTP %d", status)
	}
	var list []listEntry
	if err := json.Unmarshal(body, &list); err != nil {
		return fail("list is not a JSON array: %v", err)
	}
	snap.Source.RecordsInFeed = len(list)

	// Filter first so only wanted detail documents are fetched.
	var wanted []listEntry
	for _, e := range list {
		if f.match(e) {
			wanted = append(wanted, e)
		}
	}

	type result struct {
		tfr   model.TFR
		note  *note
		fetch error // detail could not be fetched
		parse error // detail fetched but could not be parsed
	}
	results := make([]result, len(wanted))
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for i, e := range wanted {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, e listEntry) {
			defer wg.Done()
			defer func() { <-sem }()
			r := result{note: &note{}}
			r.tfr = model.TFR{
				ID: strings.TrimSpace(e.NotamID), Facility: e.Facility, State: e.State, Type: e.Type,
				Description: e.Description, GID: e.GID, LastModified: e.ModAbsTime, New: strings.EqualFold(e.IsNew, "Y"),
			}
			xmlBody, st, _, err := c.get(ctx, c.BaseURL+detailPath(e.NotamID), maxDetail)
			switch {
			case err != nil:
				r.fetch = err
			case st != http.StatusOK:
				r.fetch = fmt.Errorf("HTTP %d", st)
			default:
				if err := parseDetail(xmlBody, &r.tfr, r.note); err != nil {
					r.parse = err
				}
			}
			results[i] = r
		}(i, e)
	}
	wg.Wait()

	var fetchFail, parseFail, noGeom, partGeom, sched, noAlt []string
	for i, r := range results {
		t := r.tfr
		raw, _ := json.Marshal(wanted[i])
		if !t.DetailRetrieved || t.RawText == "" {
			// Keep the list entry verbatim as the raw record.
			t.RawText, t.RawFormat = string(raw), model.RawUpstreamJSON
		}
		if r.fetch != nil || r.parse != nil {
			t.DetailRetrieved = false
			what := r.fetch
			if what == nil {
				what = r.parse
			}
			r.note.Add("detail document unavailable: %v; only the list entry is shown", what)
		}
		switch {
		case r.fetch != nil:
			fetchFail = append(fetchFail, t.ID)
		case r.parse != nil || len(r.note.Issues) > 0:
			parseFail = append(parseFail, t.ID)
		}
		t.ParseIssues = r.note.Issues
		if t.DetailRetrieved {
			withGeom := 0
			for _, a := range t.Areas {
				if a.Geometry != nil {
					withGeom++
				}
				if a.Scheduled {
					sched = append(sched, t.ID)
				}
			}
			switch {
			case withGeom == 0:
				noGeom = append(noGeom, t.ID)
			case withGeom < len(t.Areas):
				partGeom = append(partGeom, t.ID)
			}
			for _, a := range t.Areas {
				if !a.Lower.Known() || !a.Upper.Known() {
					noAlt = append(noAlt, t.ID)
					break
				}
			}
		}
		if err := t.Validate(); err != nil {
			t = model.TFR{ID: fmt.Sprintf("%s#%d", SourceID, i), RawText: string(raw), RawFormat: model.RawUpstreamJSON,
				ParseIssues: append(t.ParseIssues, "record failed validation: "+err.Error())}
			parseFail = append(parseFail, t.ID)
		}
		snap.Items = append(snap.Items, t)
	}
	snap.Source.RecordsReturned = len(snap.Items)

	snap.Issues = source.AppendIssue(snap.Issues, SourceID, provenance.UpstreamError, provenance.Warning, fetchFail,
		fmt.Sprintf("%d TFR detail document(s) could not be fetched; those TFRs show only their list entry", len(source.Unique(fetchFail))))
	snap.Issues = source.AppendIssue(snap.Issues, SourceID, provenance.ParseFailed, provenance.Warning, parseFail,
		fmt.Sprintf("%d TFR(s) had fields that could not be parsed; they are included, flagged in parse_issues", len(source.Unique(parseFail))))
	snap.Issues = source.AppendIssue(snap.Issues, SourceID, provenance.GeometryUnavailable, provenance.Warning, noGeom,
		fmt.Sprintf("%d TFR(s) publish no geometry (text-only); their restriction is described by raw_text", len(source.Unique(noGeom))))
	snap.Issues = source.AppendIssue(snap.Issues, SourceID, provenance.GeometryPartial, provenance.Warning, partGeom,
		fmt.Sprintf("%d TFR(s) have some areas with no geometry", len(source.Unique(partGeom))))
	snap.Issues = source.AppendIssue(snap.Issues, SourceID, provenance.ScheduleUnevaluable, provenance.Warning, sched,
		fmt.Sprintf("%d TFR(s) carry a schedule that the server does not interpret; see each area's schedules", len(source.Unique(sched))))
	snap.Issues = source.AppendIssue(snap.Issues, SourceID, provenance.AltitudeUnknown, provenance.Info, noAlt,
		fmt.Sprintf("%d TFR(s) have an area with an unknown vertical limit", len(source.Unique(noAlt))))
	return snap, nil
}
