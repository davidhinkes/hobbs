package mcpserver

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// HTTPConfig configures the hosted (Streamable HTTP) transport.
type HTTPConfig struct {
	// Tokens are the accepted bearer tokens, one per agent or user. At least
	// one is required: the hosted server never runs unauthenticated.
	Tokens []string
	// RatePerSecond and Burst bound total requests so a runaway agent loop
	// cannot exhaust upstream quotas. Zero values use the defaults.
	RatePerSecond float64
	Burst         int
	Now           func() time.Time
}

// limiter is a small global token bucket.
type limiter struct {
	mu     sync.Mutex
	tokens float64
	last   time.Time
	rate   float64
	burst  float64
	now    func() time.Time
}

func newLimiter(rate float64, burst int, now func() time.Time) *limiter {
	return &limiter{tokens: float64(burst), last: now(), rate: rate, burst: float64(burst), now: now}
}

// allow reports whether a request may proceed, and if not how long to wait.
func (l *limiter) allow() (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	t := l.now()
	l.tokens += t.Sub(l.last).Seconds() * l.rate
	if l.tokens > l.burst {
		l.tokens = l.burst
	}
	l.last = t
	if l.tokens >= 1 {
		l.tokens--
		return true, 0
	}
	return false, time.Duration((1 - l.tokens) / l.rate * float64(time.Second))
}

// Handler serves the MCP endpoint at /mcp and an unauthenticated /healthz.
func Handler(srv *mcp.Server, cfg HTTPConfig) (http.Handler, error) {
	var toks [][32]byte
	for _, t := range cfg.Tokens {
		if t = strings.TrimSpace(t); t != "" {
			if len(t) < 16 {
				return nil, errors.New("mcpserver: bearer tokens must be at least 16 characters")
			}
			toks = append(toks, sha256.Sum256([]byte(t)))
		}
	}
	if len(toks) == 0 {
		return nil, errors.New("mcpserver: at least one bearer token is required for the HTTP transport")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.RatePerSecond <= 0 {
		cfg.RatePerSecond = 5
	}
	if cfg.Burst <= 0 {
		cfg.Burst = 10
	}
	lim := newLimiter(cfg.RatePerSecond, cfg.Burst, cfg.Now)

	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})

	authed := func(r *http.Request) bool {
		h := r.Header.Get("Authorization")
		if len(h) < 8 || !strings.EqualFold(h[:7], "bearer ") {
			return false
		}
		got := sha256.Sum256([]byte(strings.TrimSpace(h[7:])))
		ok := 0
		for _, t := range toks { // compare against every token: no early exit
			ok |= subtle.ConstantTimeCompare(got[:], t[:])
		}
		return ok == 1
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		if !authed(r) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="hobbs"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if ok, wait := lim.allow(); !ok {
			w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
			http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
			return
		}
		mcpHandler.ServeHTTP(w, r)
	})
	return mux, nil
}
