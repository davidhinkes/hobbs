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

const goodToken = "test-token-0123456789abcdef"

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

func hosted(t *testing.T, cfg HTTPConfig) (*httptest.Server, *upstreams) {
	t.Helper()
	u := newUpstreams(t)
	srv, err := New(u.deps(t, true, true))
	if err != nil {
		t.Fatal(err)
	}
	h, err := Handler(srv, cfg)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return ts, u
}

func do(t *testing.T, method, url, auth, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestHandlerRefusesToRunWithoutUsableTokens(t *testing.T) {
	srv, _ := New(newUpstreams(t).deps(t, false, false))
	for name, toks := range map[string][]string{"none": nil, "blank": {"", "  "}, "too short": {"short"}, "one too short": {goodToken, "short"}} {
		if _, err := Handler(srv, HTTPConfig{Tokens: toks}); err == nil {
			t.Errorf("%s: the hosted server must never run unauthenticated or with weak tokens", name)
		}
	}
	if _, err := Handler(srv, HTTPConfig{Tokens: []string{goodToken, "another-token-0123456789"}}); err != nil {
		t.Error(err)
	}
}

func TestAuthentication(t *testing.T) {
	ts, u := hosted(t, HTTPConfig{Tokens: []string{goodToken, "second-token-0123456789"}, RatePerSecond: 100})
	initialize := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`

	for name, auth := range map[string]string{
		"no header":      "",
		"wrong token":    "Bearer wrong-token-0123456789",
		"basic scheme":   "Basic " + goodToken,
		"no scheme":      goodToken,
		"empty bearer":   "Bearer ",
		"prefix of real": "Bearer " + goodToken[:20],
		"extended real":  "Bearer " + goodToken + "x",
	} {
		t.Run("rejects "+name, func(t *testing.T) {
			resp := do(t, "POST", ts.URL+"/mcp", auth, initialize)
			if resp.StatusCode != 401 || !strings.Contains(resp.Header.Get("WWW-Authenticate"), "Bearer") {
				t.Errorf("status = %d, www-authenticate = %q", resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
			}
		})
	}
	for name, auth := range map[string]string{"first": "Bearer " + goodToken, "second": "Bearer second-token-0123456789", "lowercase scheme": "bearer " + goodToken} {
		t.Run("accepts "+name, func(t *testing.T) {
			resp := do(t, "POST", ts.URL+"/mcp", auth, initialize)
			if resp.StatusCode != 200 {
				b, _ := io.ReadAll(resp.Body)
				t.Errorf("status = %d: %s", resp.StatusCode, b)
			}
		})
	}
	// Rejected requests must not reach any upstream.
	if len(u.lastNotamQuery) != 0 || len(u.awcCalls) != 0 {
		t.Errorf("unauthenticated traffic reached the sources: %v %v", u.lastNotamQuery, u.awcCalls)
	}
	// Health is open, and says nothing about sources (there is no cache to report).
	resp := do(t, "GET", ts.URL+"/healthz", "", "")
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || strings.TrimSpace(string(b)) != "ok" {
		t.Errorf("healthz = %d %q", resp.StatusCode, b)
	}
	// Unknown paths are not served.
	if resp := do(t, "GET", ts.URL+"/", "Bearer "+goodToken, ""); resp.StatusCode != 404 {
		t.Errorf("/ = %d", resp.StatusCode)
	}
}

func TestRateLimit(t *testing.T) {
	c := &clock{t: fixedNow}
	ts, _ := hosted(t, HTTPConfig{Tokens: []string{goodToken}, RatePerSecond: 2, Now: c.now}) // burst = 2 x rate = 4
	hit := func() *http.Response {
		return do(t, "POST", ts.URL+"/mcp", "Bearer "+goodToken, `{"jsonrpc":"2.0","id":1,"method":"ping"}`)
	}
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
	// Unauthenticated requests do not consume the budget.
	c.advance(2 * time.Second)
	for i := 0; i < 20; i++ {
		do(t, "POST", ts.URL+"/mcp", "", "{}")
	}
	if r := hit(); r.StatusCode == 429 {
		t.Error("bad-auth traffic must not drain the limiter for legitimate callers")
	}
}

type bearer struct{ token string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	if b.token != "" {
		r.Header.Set("Authorization", "Bearer "+b.token)
	}
	return http.DefaultTransport.RoundTrip(r)
}

func TestEndToEndOverHTTP(t *testing.T) {
	ts, _ := hosted(t, HTTPConfig{Tokens: []string{goodToken}, RatePerSecond: 100})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// A real MCP client with the token: lists tools and gets data over the wire.
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "0"}, nil).Connect(ctx,
		&mcp.StreamableClientTransport{Endpoint: ts.URL + "/mcp", HTTPClient: &http.Client{Transport: bearer{goodToken}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	tools, err := cs.ListTools(ctx, nil)
	if err != nil || len(tools.Tools) != 3 {
		t.Fatalf("tools = %v err=%v", tools, err)
	}
	res, r := call(t, cs, "get_weather_advisories", map[string]any{"kinds": []string{"CWA"}})
	if res.IsError || len(r.Provenance.Sources) != 1 || r.Disclaimer == "" {
		t.Errorf("isError=%v %+v", res.IsError, r.Provenance.Sources)
	}

	// Without the token the client cannot even connect.
	if _, err := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "0"}, nil).Connect(ctx,
		&mcp.StreamableClientTransport{Endpoint: ts.URL + "/mcp", HTTPClient: &http.Client{Transport: bearer{""}}}, nil); err == nil {
		t.Error("a client with no token must not connect")
	}
}
