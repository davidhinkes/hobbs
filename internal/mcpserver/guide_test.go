package mcpserver

import (
	"context"
	"encoding/json"
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
		if want := (Deps{Airworthy: airworthy}).describe(guideText); got != want {
			t.Errorf("airworthy=%v: response is not the whole guide (len %d, want %d)", airworthy, len(got), len(want))
		}
		if strings.HasPrefix(got, "NOT AIRWORTHY") == airworthy {
			t.Errorf("airworthy=%v: NOT AIRWORTHY prefix is wrong: %.60q", airworthy, got)
		}
	}
}

// The guide names real tools and fields; a rename in the server must break this test.
func TestBriefingGuideNamesWhatExists(t *testing.T) {
	cs := connect(t, newUpstreams(t).deps(t, true, true))
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var dataTool *mcp.Tool
	for _, tl := range res.Tools {
		if tl.Name == "get_flight_briefing_data" {
			dataTool = tl
		}
	}
	if dataTool == nil {
		t.Fatal("the data tool the guide names is not registered")
	}
	schema, _ := json.Marshal(dataTool.InputSchema)
	for _, prop := range []string{`"airports"`, `"centers"`} {
		if !strings.Contains(string(schema), prop) {
			t.Errorf("data tool input has no %s property, but the guide names it", prop)
		}
	}
	for _, must := range []string{"get_flight_briefing_data", "`centers`", "`completeness.status`", "`airworthy`", "up to 10", "go/no-go"} {
		if !strings.Contains(guideText, must) {
			t.Errorf("guide lacks %q", must)
		}
	}
	// The envelope fields the guide tells the agent to read are real.
	_, r := call(t, cs, "get_flight_briefing_data", map[string]any{"airports": []string{"KIAD"}, "include": []string{"cwas"}})
	if r.Airworthy == nil || r.Completeness.Status == "" {
		t.Error("the envelope has no airworthy or completeness.status field, but the guide names them")
	}
}

func TestBriefingGuideIgnoresUnexpectedArguments(t *testing.T) {
	cs := connect(t, newUpstreams(t).deps(t, true, true))
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "get_briefing_guide", Arguments: map[string]any{"airports": []string{"KSMO"}}})
	if err != nil || res.IsError {
		t.Fatalf("unexpected arguments must not stop the guide: err=%v isError=%v %s", err, res != nil && res.IsError, text(res))
	}
	if !strings.Contains(text(res), "# Briefing a pilot with hobbs") {
		t.Error("the guide was not returned")
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
