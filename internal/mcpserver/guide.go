package mcpserver

import (
	"context"
	_ "embed"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// guideText is the briefing guide: advice to the calling agent on how to ask a
// pilot, what to fetch and how to cut the result to a short briefing. It is
// kept as a Markdown file so it can be read and edited as a document.
//
//go:embed guide.md
var guideText string

// GuideInput has no fields.
type GuideInput struct{}

// registerGuide adds get_briefing_guide.
//
// Its response is plain text, not a provenance envelope: the guide is hobbs's
// own advice on method, not data from a source, and provenance.New refuses an
// envelope with no source (docs/superpowers/specs/2026-11-25-briefing-guide-design.md
// §5.1). It still carries the NOT AIRWORTHY sentence when the server is not
// airworthy. It advises on method only; the server still judges no record.
func registerGuide(srv *mcp.Server, d Deps) {
	ann := readOnly("How to brief a pilot with this server's data")
	ann.OpenWorldHint = ptr(false) // it reaches nothing outside the server
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "get_briefing_guide",
		Annotations: ann,
		Description: d.describe("Returns hobbs's short guide to briefing a general aviation pilot with this server's data: what to ask the pilot, what to fetch, and how to cut the result to a short briefing without losing the data-quality warnings. Call it once before briefing a pilot. It is advice on method, written by the hobbs project, not data from the FAA or NWS, and it fetches nothing."),
	}, func(context.Context, *mcp.CallToolRequest, GuideInput) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: d.describe(guideText)}},
		}, nil, nil
	})
}
