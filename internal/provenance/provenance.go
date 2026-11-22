// Package provenance defines the response envelope every tool must return.
//
// The envelope states which official sources were used, when they were
// retrieved, and every problem with the data pull that could affect the
// result. Handlers cannot return bare data: New is the only way to build an
// Envelope, and it validates and derives the completeness status itself.
package provenance

import (
	"errors"
	"fmt"
	"sort"
	"time"
)

// Disclaimer is attached to every envelope.
const Disclaimer = "Advisory only. Not an official briefing; consult official FAA/NWS sources before flight."

// clockSkewTolerance is how far an upstream timestamp may be ahead of our
// clock before CLOCK_SKEW is reported.
const clockSkewTolerance = 5 * time.Minute

type Documentation string

const (
	Documented   Documentation = "documented"
	Undocumented Documentation = "undocumented"
)

type ServedFrom string

const (
	Live       ServedFrom = "live"
	Cache      ServedFrom = "cache"
	StaleCache ServedFrom = "stale_cache"
)

type Severity string

const (
	Info     Severity = "info"
	Warning  Severity = "warning"
	Critical Severity = "critical"
)

type Status string

const (
	Complete   Status = "complete"
	Degraded   Status = "degraded"
	Incomplete Status = "incomplete"
)

// Code identifies a kind of data-quality issue.
type Code string

const (
	SourceUnavailable    Code = "SOURCE_UNAVAILABLE"
	StaleData            Code = "STALE_DATA"
	UpstreamError        Code = "UPSTREAM_ERROR"
	ParseFailed          Code = "PARSE_FAILED"
	GeometryUnavailable  Code = "GEOMETRY_UNAVAILABLE"
	GeometryPartial      Code = "GEOMETRY_PARTIAL"
	GeometryNotArea      Code = "GEOMETRY_NOT_AREA"
	AltitudeUnknown      Code = "ALTITUDE_UNKNOWN"
	ScheduleUnevaluable  Code = "SCHEDULE_UNEVALUABLE"
	UpstreamFilterIgnore Code = "UPSTREAM_FILTER_IGNORED"
	UndocumentedSource   Code = "UNDOCUMENTED_SOURCE"
	ResultTruncated      Code = "RESULT_TRUNCATED"
	SourceNotEnabled     Code = "SOURCE_NOT_ENABLED"
	ClockSkew            Code = "CLOCK_SKEW"
	AltitudeAssumed      Code = "ALTITUDE_ASSUMED"
)

var knownCodes = map[Code]bool{
	SourceUnavailable: true, StaleData: true, UpstreamError: true, ParseFailed: true,
	GeometryUnavailable: true, GeometryPartial: true, GeometryNotArea: true,
	AltitudeUnknown: true, ScheduleUnevaluable: true, UpstreamFilterIgnore: true,
	UndocumentedSource: true, ResultTruncated: true, SourceNotEnabled: true, ClockSkew: true, AltitudeAssumed: true,
}

// Source describes one official upstream consulted for a response.
type Source struct {
	ID            string        `json:"id"`
	Authority     string        `json:"authority"`
	URL           string        `json:"url"`
	Documentation Documentation `json:"documentation"`
	// RetrievedAt is when this server fetched the data.
	RetrievedAt time.Time `json:"retrieved_at"`
	// UpstreamTimestamp is the newest issue/receipt time found in the data.
	// Nil when the upstream gives none.
	UpstreamTimestamp *time.Time `json:"upstream_timestamp,omitempty"`
	// AgeSeconds is evaluated_at minus retrieved_at. Set by New.
	AgeSeconds int64 `json:"age_seconds"`
	// UpstreamAgeSeconds is evaluated_at minus upstream_timestamp. Set by New.
	UpstreamAgeSeconds *int64     `json:"upstream_age_seconds,omitempty"`
	ServedFrom         ServedFrom `json:"served_from"`
	HTTPStatus         int        `json:"http_status,omitempty"`
	ETag               string     `json:"etag,omitempty"`
	ContentSHA256      string     `json:"content_sha256,omitempty"`
	RecordsInFeed      int        `json:"records_in_feed"`
	RecordsReturned    int        `json:"records_returned"`
}

// NotConsulted names a source the query would normally use but did not.
// Any entry makes the response incomplete: list only sources that matter to
// the query.
type NotConsulted struct {
	ID     string `json:"id"`
	Reason string `json:"reason"`
}

// Issue is a data-quality problem that may affect the result.
type Issue struct {
	Code     Code     `json:"code"`
	Severity Severity `json:"severity"`
	Source   string   `json:"source,omitempty"`
	Message  string   `json:"message"`
	Affects  []string `json:"affects,omitempty"`
	Refs     []string `json:"refs,omitempty"`
}

type Provenance struct {
	Sources      []Source       `json:"sources"`
	NotConsulted []NotConsulted `json:"not_consulted"`
}

type Completeness struct {
	Status Status  `json:"status"`
	Issues []Issue `json:"issues"`
}

// Envelope wraps every tool result.
type Envelope[T any] struct {
	Data         T            `json:"data"`
	EvaluatedAt  time.Time    `json:"evaluated_at"`
	Provenance   Provenance   `json:"provenance"`
	Completeness Completeness `json:"completeness"`
	Disclaimer   string       `json:"disclaimer"`
}

// CanAssertNone reports whether an empty result may be presented as "nothing
// found". It is true only when nothing about the data pull is in doubt.
func (c Completeness) CanAssertNone() bool { return c.Status == Complete }

// New builds a validated Envelope. It derives ages, adds issues implied by the
// sources (stale cache, undocumented endpoint, clock skew, not consulted) and
// computes the completeness status. It returns an error if the provenance is
// missing or malformed, so a handler cannot ship an unattributed result.
func New[T any](data T, evaluatedAt time.Time, sources []Source, notConsulted []NotConsulted, issues []Issue) (Envelope[T], error) {
	var zero Envelope[T]
	if evaluatedAt.IsZero() {
		return zero, errors.New("provenance: evaluatedAt is required")
	}
	evaluatedAt = evaluatedAt.UTC()
	if len(sources) == 0 && len(notConsulted) == 0 {
		return zero, errors.New("provenance: at least one source or not-consulted entry is required")
	}

	all := make([]Issue, 0, len(issues)+len(sources))
	for _, is := range issues {
		if !knownCodes[is.Code] {
			return zero, fmt.Errorf("provenance: unknown issue code %q", is.Code)
		}
		if is.Severity != Info && is.Severity != Warning && is.Severity != Critical {
			return zero, fmt.Errorf("provenance: issue %s has invalid severity %q", is.Code, is.Severity)
		}
		if is.Message == "" {
			return zero, fmt.Errorf("provenance: issue %s has no message", is.Code)
		}
		all = append(all, is)
	}
	has := func(code Code, src string) bool {
		for _, is := range all {
			if is.Code == code && is.Source == src {
				return true
			}
		}
		return false
	}

	srcs := make([]Source, 0, len(sources))
	seen := map[string]bool{}
	for _, s := range sources {
		switch {
		case s.ID == "" || s.URL == "" || s.Authority == "":
			return zero, fmt.Errorf("provenance: source %q needs id, authority and url", s.ID)
		case seen[s.ID]:
			return zero, fmt.Errorf("provenance: duplicate source %q", s.ID)
		case s.RetrievedAt.IsZero():
			return zero, fmt.Errorf("provenance: source %s has no retrieved_at", s.ID)
		case s.Documentation != Documented && s.Documentation != Undocumented:
			return zero, fmt.Errorf("provenance: source %s has invalid documentation %q", s.ID, s.Documentation)
		case s.ServedFrom != Live && s.ServedFrom != Cache && s.ServedFrom != StaleCache:
			return zero, fmt.Errorf("provenance: source %s has invalid served_from %q", s.ID, s.ServedFrom)
		}
		seen[s.ID] = true

		s.RetrievedAt = s.RetrievedAt.UTC()
		s.AgeSeconds = int64(evaluatedAt.Sub(s.RetrievedAt).Seconds())
		if s.UpstreamTimestamp != nil {
			ut := s.UpstreamTimestamp.UTC()
			s.UpstreamTimestamp = &ut
			age := int64(evaluatedAt.Sub(ut).Seconds())
			s.UpstreamAgeSeconds = &age
			if ut.After(evaluatedAt.Add(clockSkewTolerance)) && !has(ClockSkew, s.ID) {
				all = append(all, Issue{Code: ClockSkew, Severity: Warning, Source: s.ID,
					Message: fmt.Sprintf("upstream timestamp %s is ahead of our clock", ut.Format(time.RFC3339))})
			}
		}
		if s.ServedFrom == StaleCache && !has(StaleData, s.ID) {
			all = append(all, Issue{Code: StaleData, Severity: Warning, Source: s.ID,
				Message: fmt.Sprintf("served from stale cache, data retrieved %d s ago", s.AgeSeconds)})
		}
		if s.Documentation == Undocumented && !has(UndocumentedSource, s.ID) {
			all = append(all, Issue{Code: UndocumentedSource, Severity: Info, Source: s.ID,
				Message: "endpoint is not a documented, supported API and may change without notice"})
		}
		srcs = append(srcs, s)
	}

	nc := make([]NotConsulted, 0, len(notConsulted))
	for _, n := range notConsulted {
		if n.ID == "" || n.Reason == "" {
			return zero, errors.New("provenance: not-consulted entries need id and reason")
		}
		nc = append(nc, n)
	}

	sort.SliceStable(srcs, func(i, j int) bool { return srcs[i].ID < srcs[j].ID })
	sort.SliceStable(nc, func(i, j int) bool { return nc[i].ID < nc[j].ID })
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].Severity != all[j].Severity {
			return severityRank(all[i].Severity) > severityRank(all[j].Severity)
		}
		if all[i].Source != all[j].Source {
			return all[i].Source < all[j].Source
		}
		return all[i].Code < all[j].Code
	})

	return Envelope[T]{
		Data:         data,
		EvaluatedAt:  evaluatedAt,
		Provenance:   Provenance{Sources: srcs, NotConsulted: nc},
		Completeness: Completeness{Status: deriveStatus(all, nc, srcs), Issues: all},
		Disclaimer:   Disclaimer,
	}, nil
}

func severityRank(s Severity) int {
	switch s {
	case Critical:
		return 2
	case Warning:
		return 1
	}
	return 0
}

// deriveStatus: incomplete if a source was not consulted or any critical issue
// exists; degraded if any warning exists or a source was served from stale
// cache; otherwise complete.
func deriveStatus(issues []Issue, nc []NotConsulted, srcs []Source) Status {
	if len(nc) > 0 {
		return Incomplete
	}
	st := Complete
	for _, is := range issues {
		switch is.Severity {
		case Critical:
			return Incomplete
		case Warning:
			st = Degraded
		}
	}
	for _, s := range srcs {
		if s.ServedFrom == StaleCache {
			st = Degraded
		}
	}
	return st
}
