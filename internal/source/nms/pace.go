package nms

import (
	"context"
	"sync"
	"time"
)

// pacer spaces requests to the NMS-API at most rate per second, with no
// burst. Callers over the rate wait their turn rather than fail, so a
// briefing that asks for many locations at once is slowed, not refused. The
// FAA's NMS-API FAQ (received 2026-11-22) gives the test environment a limit
// of 1 request per second.
type pacer struct {
	mu       sync.Mutex
	interval time.Duration
	next     time.Time // earliest time the next request may go
	now      func() time.Time
	sleep    func(context.Context, time.Duration) error
}

func newPacer(rate float64, now func() time.Time, sleep func(context.Context, time.Duration) error) *pacer {
	return &pacer{interval: time.Duration(float64(time.Second) / rate), now: now, sleep: sleep}
}

// wait blocks until the caller's turn. A nil pacer never waits.
func (p *pacer) wait(ctx context.Context) error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	t := p.now()
	slot := p.next
	if slot.Before(t) {
		slot = t
	}
	p.next = slot.Add(p.interval)
	p.mu.Unlock()
	if d := slot.Sub(t); d > 0 {
		return p.sleep(ctx, d)
	}
	return ctx.Err()
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	tm := time.NewTimer(d)
	defer tm.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-tm.C:
		return nil
	}
}
