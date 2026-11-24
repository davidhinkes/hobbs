package mcpserver

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func hosted(t *testing.T, cfg HTTPConfig) *httptest.Server {
	t.Helper()
	srv, err := New(newUpstreams(t).deps(t, true, false))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(Handler(srv, cfg))
	t.Cleanup(ts.Close)
	return ts
}

func post(t *testing.T, url, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("POST", url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestRoutes(t *testing.T) {
	ts := hosted(t, HTTPConfig{RatePerSecond: 100})
	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || strings.TrimSpace(string(b)) != "ok" {
		t.Errorf("healthz = %d %q", resp.StatusCode, b)
	}
	// The MCP endpoint is open: no credentials are needed.
	if r := post(t, ts.URL+"/mcp", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`); r.StatusCode != 200 {
		t.Errorf("/mcp = %d", r.StatusCode)
	}
	// Unknown paths are not served.
	if resp, _ := http.Get(ts.URL + "/"); resp.StatusCode != 404 {
		t.Errorf("/ = %d", resp.StatusCode)
	}
}

func TestRateLimit(t *testing.T) {
	c := &clock{t: fixedNow}
	ts := hosted(t, HTTPConfig{RatePerSecond: 2, Now: c.now}) // burst = 2 x rate = 4
	hit := func() *http.Response { return post(t, ts.URL+"/mcp", `{"jsonrpc":"2.0","id":1,"method":"ping"}`) }
	for i := 0; i < 4; i++ {
		if r := hit(); r.StatusCode == 429 {
			t.Fatalf("request %d limited inside the burst", i+1)
		}
	}
	r := hit()
	if r.StatusCode != 429 || r.Header.Get("Retry-After") == "" {
		t.Fatalf("status = %d retry-after = %q", r.StatusCode, r.Header.Get("Retry-After"))
	}
	c.advance(600 * time.Millisecond) // 1.2 tokens at 2/s
	if r := hit(); r.StatusCode == 429 {
		t.Error("should be allowed after tokens refill")
	}
	if r := hit(); r.StatusCode != 429 {
		t.Error("only one token had refilled")
	}
	// Health checks are not rate limited.
	for i := 0; i < 10; i++ {
		if resp, _ := http.Get(ts.URL + "/healthz"); resp.StatusCode != 200 {
			t.Fatal("healthz must not be rate limited")
		}
	}
}

func TestEndToEndOverHTTP(t *testing.T) {
	ts := hosted(t, HTTPConfig{RatePerSecond: 100})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "0"}, nil).Connect(ctx,
		&mcp.StreamableClientTransport{Endpoint: ts.URL + "/mcp"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	tools, err := cs.ListTools(ctx, nil)
	if err != nil || len(tools.Tools) != 4 {
		t.Fatalf("tools = %v err=%v", tools, err)
	}
	res, r := call(t, cs, "get_weather_advisories", map[string]any{"kinds": []string{"CWA"}})
	if res.IsError || len(r.Provenance.Sources) != 1 || r.Disclaimer == "" {
		t.Errorf("isError=%v %+v", res.IsError, r.Provenance.Sources)
	}
}
