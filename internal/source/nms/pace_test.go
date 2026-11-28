package nms

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// fakeClock is a manual clock: sleeping advances it.
type fakeClock struct {
	mu    sync.Mutex
	t     time.Time
	slept []time.Duration
}

func (c *fakeClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.slept = append(c.slept, d)
	c.t = c.t.Add(d)
	return nil
}

func TestPacerSpacesRequests(t *testing.T) {
	clk := &fakeClock{t: fixedNow}
	p := newPacer(1, clk.now, clk.sleep)
	for i := 0; i < 3; i++ {
		if err := p.wait(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	// The first request goes at once; each later one waits its second.
	want := []time.Duration{time.Second, time.Second}
	if len(clk.slept) != len(want) || clk.slept[0] != want[0] || clk.slept[1] != want[1] {
		t.Fatalf("slept %v, want %v", clk.slept, want)
	}
}

func TestPacerDoesNotWaitAfterIdle(t *testing.T) {
	clk := &fakeClock{t: fixedNow}
	p := newPacer(1, clk.now, clk.sleep)
	_ = p.wait(context.Background())
	clk.t = clk.t.Add(5 * time.Second) // idle: no saved-up burst either
	_ = p.wait(context.Background())
	_ = p.wait(context.Background())
	if len(clk.slept) != 1 || clk.slept[0] != time.Second {
		t.Fatalf("slept %v, want one 1s wait (no burst after idle)", clk.slept)
	}
}

func TestPacerHonorsContext(t *testing.T) {
	clk := &fakeClock{t: fixedNow}
	p := newPacer(1, clk.now, clk.sleep)
	_ = p.wait(context.Background())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := p.wait(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestNilPacerNeverWaits(t *testing.T) {
	var p *pacer
	if err := p.wait(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestNewClientRejectsNegativeRate(t *testing.T) {
	if _, err := NewClient(Config{Host: "https://x.example", ClientID: "a", ClientSecret: "b", RatePerSecond: -1}); err == nil {
		t.Fatal("negative rate accepted")
	}
}

// Every request the client sends, token and ping included, goes through the
// pacer: with concurrent callers, no two requests reach the API in the same
// interval.
func TestClientPacesEveryRequest(t *testing.T) {
	var mu sync.Mutex
	var at []time.Time
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		at = append(at, time.Now())
		mu.Unlock()
		if r.URL.Path == "/v1/auth/token" {
			w.Write([]byte(`{"access_token":"tok","expires_in":"1799"}`))
			return
		}
		w.Write([]byte(`{"status":"Success","data":{}}`))
	}))
	defer srv.Close()
	const rate = 20 // 50 ms apart keeps the test fast
	c, err := NewClient(Config{Host: srv.URL, ClientID: testID, ClientSecret: testSecret, RatePerSecond: rate})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.Query(context.Background(), Query{Location: "KSMO"}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if _, err := c.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(at) != 6 { // one token, four queries, one ping
		t.Fatalf("%d requests, want 6", len(at))
	}
	min := time.Second / rate * 9 / 10 // allow scheduler jitter
	for i := 1; i < len(at); i++ {
		if gap := at[i].Sub(at[i-1]); gap < min {
			t.Errorf("requests %d and %d only %v apart, want at least %v", i-1, i, gap, min)
		}
	}
}
