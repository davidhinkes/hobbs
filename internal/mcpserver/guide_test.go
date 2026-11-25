package mcpserver

import (
	"context"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestBriefingGuideIsReturnedAsText(t *testing.T) {
	for _, airworthy := range []bool{false, true} {
		cs := connect(t, newUpstreams(t).deps(t, true, airworthy))
		res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "get_briefing_guide"})
		if err != nil || res.IsError {
			t.Fatalf("airworthy=%v: err=%v isError=%v", airworthy, err, res != nil && res.IsError)
		}
		got := text(res)
		if !strings.Contains(got, "# Briefing a pilot with hobbs") {
			t.Errorf("airworthy=%v: guide heading missing", airworthy)
		}
		if strings.HasPrefix(got, "NOT AIRWORTHY") == airworthy {
			t.Errorf("airworthy=%v: NOT AIRWORTHY prefix is wrong: %.60q", airworthy, got)
		}
	}
}

// The guide names real tools and fields; a rename must break this test.
func TestBriefingGuideNamesWhatExists(t *testing.T) {
	for _, must := range []string{"get_flight_briefing_data", "`centers`", "`completeness.status`", "`airworthy`", "up to 10", "go/no-go"} {
		if !strings.Contains(guideText, must) {
			t.Errorf("guide lacks %q", must)
		}
	}
}

func TestBriefingGuideIgnoresUnexpectedArguments(t *testing.T) {
	cs := connect(t, newUpstreams(t).deps(t, true, true))
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "get_briefing_guide", Arguments: map[string]any{"airports": []string{"KSMO"}}})
	if err != nil {
		t.Fatalf("protocol error: %v", err)
	}
	if !res.IsError && !strings.Contains(text(res), "# Briefing a pilot with hobbs") {
		t.Error("neither the guide nor an input error")
	}
}

func TestInstructionsAndDataToolPointAtTheGuide(t *testing.T) {
	cs := connect(t, newUpstreams(t).deps(t, true, true))
	const want = "Before briefing a pilot, call get_briefing_guide once and follow it."
	if got := cs.InitializeResult().Instructions; !strings.Contains(got, want) {
		t.Errorf("instructions lack the pointer: %q", got)
	}
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tl := range res.Tools {
		if tl.Name == "get_flight_briefing_data" && !strings.Contains(tl.Description, want) {
			t.Errorf("get_flight_briefing_data description lacks the pointer")
		}
	}
}
