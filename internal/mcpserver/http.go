package mcpserver

import (
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// HTTPConfig configures the hosted (Streamable HTTP) transport. The endpoint
// is open: there is no authentication.
type HTTPConfig struct {
	// RatePerSecond bounds total requests so a runaway agent loop cannot
	// exhaust upstream quotas; bursts of up to twice the rate are allowed.
	// Zero uses the default.
	RatePerSecond float64
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

// Handler serves the MCP endpoint at /mcp, behind a global rate limit, and
// /healthz.
func Handler(srv *mcp.Server, cfg HTTPConfig) http.Handler {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.RatePerSecond <= 0 {
		cfg.RatePerSecond = 5
	}
	lim := newLimiter(cfg.RatePerSecond, int(math.Ceil(2*cfg.RatePerSecond)), cfg.Now)

	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		if ok, wait := lim.allow(); !ok {
			w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
			http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
			return
		}
		mcpHandler.ServeHTTP(w, r)
	})
	return mux
}
