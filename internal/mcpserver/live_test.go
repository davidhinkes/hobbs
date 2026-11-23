//go:build live

package mcpserver

import (
	"context"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/davidhinkes/hobbs/internal/source/nms"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestLiveStdio builds the real binary and drives it over stdio against the
// real sources, the way an MCP client would. Built only with -tags live; NOTAMs
// are included when HOBBS_NMS_CLIENT_ID and HOBBS_NMS_CLIENT_SECRET are set. Nothing secret
// is printed.
func TestLiveStdio(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "hobbs")
	if out, err := exec.Command("go", "build", "-o", bin, "../../cmd/hobbs").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	args := []string{}
	if _, _, ok := nms.CredentialsFromEnv(); ok {
		args = append(args, "-nms-url", nms.StagingHost)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "live", Version: "0"}, nil).Connect(ctx, &mcp.CommandTransport{Command: exec.Command(bin, args...)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()

	tools, _ := cs.ListTools(ctx, nil)
	names := []string{}
	for _, tl := range tools.Tools {
		names = append(names, tl.Name)
	}
	t.Logf("tools over stdio: %v", names)

	summarize := func(name string, args map[string]any) {
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Errorf("%s: %v", name, err)
			return
		}
		b, _ := json.Marshal(res.StructuredContent)
		var r reply
		json.Unmarshal(b, &r)
		var d struct {
			Advisories []json.RawMessage `json:"advisories"`
			NOTAMs     []json.RawMessage `json:"notams"`
		}
		json.Unmarshal(r.Data, &d)
		t.Logf("%s %v -> isError=%v status=%s advisories=%d notams=%d sources=%d bytes=%d", name, args, res.IsError, r.Completeness.Status, len(d.Advisories), len(d.NOTAMs), len(r.Provenance.Sources), len(b))
		for _, i := range r.Completeness.Issues {
			t.Logf("    [%s] %s %s", i.Severity, i.Code, i.Source)
		}
	}
	summarize("source_status", nil)
	summarize("get_weather_advisories", nil)
	summarize("get_weather_advisories", map[string]any{"kinds": []string{"AIRMET"}, "hazard": "TURB-HI"})
	summarize("get_notams", map[string]any{"locations": []string{"KSMO", "KEMT"}, "include_geometry": false})
	summarize("get_notams", map[string]any{"latitude": 38.95, "longitude": -77.45, "radius_nm": 10.0})
	summarize("get_notams", map[string]any{"latitude": 38.95, "longitude": -77.45, "radius_nm": 10.0, "include_geometry": false})
	summarize("get_notams", map[string]any{"latitude": 38.95, "longitude": -77.45, "radius_nm": 10.0, "include_geometry": false, "limit": 50})
	summarize("get_notams", map[string]any{"locations": []string{"KIAD"}, "classification": "DOM"}) // a correctable mistake
}
