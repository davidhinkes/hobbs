package mcpserver

import (
	"context"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/davidhinkes/hobbs/internal/model"
	"github.com/davidhinkes/hobbs/internal/provenance"
	"github.com/davidhinkes/hobbs/internal/source"
	"github.com/davidhinkes/hobbs/internal/source/awc"
	"github.com/davidhinkes/hobbs/internal/source/nms"
)

// StatusInput has no fields.
type StatusInput struct{}

// SourceStatus is one source's health, checked live.
type SourceStatus struct {
	ID            string `json:"id"`
	Authority     string `json:"authority"`
	Documentation string `json:"documentation"`
	// Environment is "production", "staging" (a test environment), "custom
	// host", or "n/a".
	Environment string `json:"environment"`
	// Status is "ok", "error" or "not_enabled".
	Status        string `json:"status"`
	HTTPStatus    int    `json:"http_status,omitempty"`
	LatencyMS     int64  `json:"latency_ms,omitempty"`
	RecordsInFeed int    `json:"records_in_feed,omitempty"`
	Error         string `json:"error,omitempty"`
}

type statusData struct {
	CheckedAt time.Time      `json:"checked_at"`
	Sources   []SourceStatus `json:"sources"`
}

func registerStatus(srv *mcp.Server, d Deps) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "source_status",
		Annotations: readOnly("Health of each data source"),
		Description: d.describe("Checks each upstream source live (the four advisory feeds, the METAR and TAF services with one request each for KLAX, and the NOTAM source) and reports whether it is reachable, how long it took, whether it is a production or test (staging) environment, and whether its endpoint is a documented API. " +
			"Use it to understand why a response was incomplete. There is no cache: every data call fetches from the source at the time you call." + reliance),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ StatusInput) (*mcp.CallToolResult, any, error) {
		return statusHandler(ctx, d)
	})
}

// statusProbeStation is the station the METAR and TAF checks ask for: one
// request each, for a large airport that always publishes both.
const statusProbeStation = "KLAX"

// statusCheck checks one source live.
type statusCheck func(ctx context.Context) (SourceStatus, provenance.Source, error)

// statusChecks are the checks, in output order: the four advisory feeds, the
// METAR and TAF services, then the NOTAM source when it is enabled.
func statusChecks(d Deps) []statusCheck {
	var checks []statusCheck
	for _, f := range weatherFeeds {
		feed := f.get(d.AWC)
		checks = append(checks, func(ctx context.Context) (SourceStatus, provenance.Source, error) {
			snap, err := feed.Fetch(ctx)
			return SourceStatus{ID: feed.ID(), Authority: awc.Authority, Environment: "n/a", RecordsInFeed: len(snap.Items)}, snap.Source, err
		})
	}
	for _, s := range []struct {
		id    string
		fetch func(*awc.Client, context.Context, []string) (source.Snapshot[model.StationReport], error)
	}{{awc.IDMETAR, (*awc.Client).METARs}, {awc.IDTAF, (*awc.Client).TAFs}} {
		checks = append(checks, func(ctx context.Context) (SourceStatus, provenance.Source, error) {
			snap, err := s.fetch(d.AWC, ctx, []string{statusProbeStation})
			return SourceStatus{ID: s.id, Authority: awc.Authority, Environment: "n/a", RecordsInFeed: len(snap.Items)}, snap.Source, err
		})
	}
	if d.NMS != nil {
		checks = append(checks, func(ctx context.Context) (SourceStatus, provenance.Source, error) {
			code, err := d.NMS.Ping(ctx)
			env := "custom host"
			switch d.NMS.Host() {
			case nms.StagingHost:
				env = "staging"
			case nms.ProductionHost:
				env = "production"
			}
			src := provenance.Source{ID: nms.SourceID, Authority: nms.Authority, URL: d.NMS.Host() + "/nmsapi/v1/ping",
				Documentation: provenance.Documented, RetrievedAt: d.Now().UTC(), ServedFrom: provenance.Live, HTTPStatus: code}
			return SourceStatus{ID: nms.SourceID, Authority: nms.Authority, Environment: env}, src, err
		})
	}
	return checks
}

func statusHandler(ctx context.Context, d Deps) (*mcp.CallToolResult, any, error) {
	now := d.Now().UTC()
	checks := statusChecks(d)
	statuses := make([]SourceStatus, len(checks))
	srcs := make([]provenance.Source, len(checks))
	errs := make([]error, len(checks))
	parallel(len(checks), func(i int) {
		start := time.Now()
		st, src, err := checks[i](ctx)
		st.LatencyMS = time.Since(start).Milliseconds()
		st.Documentation, st.Status, st.HTTPStatus = string(provenance.Documented), "ok", src.HTTPStatus
		if err != nil {
			st.Status, st.Error, errs[i] = "error", err.Error(), err
		}
		statuses[i], srcs[i] = st, src
	})

	var sources []provenance.Source
	var issues []provenance.Issue
	var notConsulted []provenance.NotConsulted
	for i := range statuses {
		if errs[i] != nil {
			s, is := failure(srcs[i], statuses[i].ID, errs[i], now)
			sources, issues = append(sources, s), append(issues, is)
			continue
		}
		sources = append(sources, srcs[i])
	}
	if d.NMS == nil {
		statuses = append(statuses, SourceStatus{ID: nms.SourceID, Authority: nms.Authority, Documentation: string(provenance.Documented),
			Environment: "n/a", Status: notEnabled, Error: notEnabledText})
		notConsulted = append(notConsulted, provenance.NotConsulted{ID: nms.SourceID, Reason: notEnabled})
	}
	return respond(d, statusData{CheckedAt: now, Sources: statuses}, now, sources, notConsulted, issues, false)
}
