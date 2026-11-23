package mcpserver

import (
	"context"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/davidhinkes/hobbs/internal/provenance"
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
	// Status is "ok", "error" or "not_configured".
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
		Description: d.describe("Checks each upstream source live and reports whether it is reachable, how long it took, whether it is a production or test (staging) environment, and whether its endpoint is a documented API. " +
			"Use it to understand why a response was incomplete. There is no cache: every data call fetches from the source at the time you call." + reliance),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ StatusInput) (*mcp.CallToolResult, any, error) {
		return statusHandler(ctx, d)
	})
}

func statusHandler(ctx context.Context, d Deps) (*mcp.CallToolResult, any, error) {
	now := d.Now().UTC()
	n := len(weatherFeeds)
	total := n
	if d.NMS != nil {
		total++
	}
	statuses := make([]SourceStatus, total)
	srcs := make([]provenance.Source, total)
	errs := make([]error, total)
	parallel(total, func(i int) {
		start := time.Now()
		if i < n {
			feed := weatherFeeds[i].get(d.AWC)
			snap, err := feed.Fetch(ctx)
			st := SourceStatus{ID: feed.ID(), Authority: awc.Authority, Documentation: string(provenance.Documented),
				Environment: "n/a", Status: "ok", HTTPStatus: snap.Source.HTTPStatus, RecordsInFeed: len(snap.Items), LatencyMS: time.Since(start).Milliseconds()}
			if err != nil {
				st.Status, st.Error, errs[i] = "error", err.Error(), err
			}
			statuses[i], srcs[i] = st, snap.Source
			return
		}
		code, err := d.NMS.Ping(ctx)
		env := "custom host"
		switch d.NMS.Host() {
		case nms.StagingHost:
			env = "staging"
		case nms.ProductionHost:
			env = "production"
		}
		st := SourceStatus{ID: nms.SourceID, Authority: nms.Authority, Documentation: string(provenance.Documented),
			Environment: env, Status: "ok", HTTPStatus: code, LatencyMS: time.Since(start).Milliseconds()}
		if err != nil {
			st.Status, st.Error, errs[i] = "error", err.Error(), err
		}
		statuses[i] = st
		srcs[i] = provenance.Source{ID: nms.SourceID, Authority: nms.Authority, URL: d.NMS.Host() + "/nmsapi/v1/ping",
			Documentation: provenance.Documented, RetrievedAt: time.Now().UTC(), ServedFrom: provenance.Live, HTTPStatus: code}
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
			Environment: "n/a", Status: "not_configured", Error: "NMS-API credentials are not configured"})
		notConsulted = append(notConsulted, provenance.NotConsulted{ID: nms.SourceID, Reason: "not_configured"})
	}
	env, err := provenance.New(statusData{CheckedAt: now, Sources: statuses}, now, sources, notConsulted, d.mark(issues))
	if err != nil {
		return toolError("internal error: %v", err)
	}
	env.Airworthy = d.Airworthy
	return envelopeResult(env, false)
}
