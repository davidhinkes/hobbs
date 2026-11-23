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
	// Environment is "production", "staging" (a test environment), or "n/a".
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
		Description: "Checks each upstream source live and reports whether it is reachable, how long it took, whether it is a production or test (staging) environment, and whether its endpoint is a documented API. " +
			"Use it to understand why a response was incomplete. There is no cache: every data call fetches from the source at the time you call." + reliance,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ StatusInput) (*mcp.CallToolResult, any, error) {
		return statusHandler(ctx, d)
	})
}

func statusHandler(ctx context.Context, d Deps) (*mcp.CallToolResult, any, error) {
	now := d.Now().UTC()
	type awcCheck struct {
		id    string
		fetch func() (provenance.Source, int, error)
	}
	checks := []awcCheck{
		{awc.IDAirSigmet, func() (provenance.Source, int, error) {
			s, e := d.AWC.AirSigmet().Fetch(ctx)
			return s.Source, len(s.Items), e
		}},
		{awc.IDGAirmet, func() (provenance.Source, int, error) {
			s, e := d.AWC.GAirmet().Fetch(ctx)
			return s.Source, len(s.Items), e
		}},
		{awc.IDIntlSig, func() (provenance.Source, int, error) {
			s, e := d.AWC.IntlSigmet().Fetch(ctx)
			return s.Source, len(s.Items), e
		}},
		{awc.IDCWA, func() (provenance.Source, int, error) {
			s, e := d.AWC.CWA().Fetch(ctx)
			return s.Source, len(s.Items), e
		}},
	}
	n := len(checks)
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
			s, count, err := checks[i].fetch()
			st := SourceStatus{ID: checks[i].id, Authority: awc.Authority, Documentation: string(provenance.Documented),
				Environment: "n/a", Status: "ok", HTTPStatus: s.HTTPStatus, RecordsInFeed: count, LatencyMS: time.Since(start).Milliseconds()}
			if err != nil {
				st.Status, st.Error, errs[i] = "error", err.Error(), err
			}
			statuses[i], srcs[i] = st, s
			return
		}
		code, err := d.NMS.Ping(ctx)
		env := "staging"
		if d.NMS.Production() {
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
		if statuses[i].ID == nms.SourceID && !d.NMS.Production() {
			issues = append(issues, provenance.Issue{Code: provenance.NonProductionSource, Severity: provenance.Warning, Source: nms.SourceID,
				Message: "the NOTAM source is a non-production (staging) environment"})
		}
	}
	if d.NMS == nil {
		statuses = append(statuses, SourceStatus{ID: nms.SourceID, Authority: nms.Authority, Documentation: string(provenance.Documented),
			Environment: "n/a", Status: "not_configured", Error: "NMS-API credentials are not configured"})
		notConsulted = append(notConsulted, provenance.NotConsulted{ID: nms.SourceID, Reason: "not_configured"})
	}
	env, err := provenance.New(statusData{CheckedAt: now, Sources: statuses}, now, sources, notConsulted, issues)
	if err != nil {
		return toolError("internal error: %v", err)
	}
	return envelopeResult(env, false)
}
