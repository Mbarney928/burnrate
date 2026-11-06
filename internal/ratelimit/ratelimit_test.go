package ratelimit

import (
	"sync"
	"testing"
	"time"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

func TestBurstThenRefill(t *testing.T) {
	c := &clock{t: time.Unix(1_700_000_000, 0)}
	l := New(2, 3, c.now) // 2/s, burst 3

	for i := range 3 {
		if !l.Allow("a") {
			t.Fatalf("request %d within burst was denied", i)
		}
	}
	if l.Allow("a") || !l.Blocked("a") {
		t.Fatal("4th immediate request should be denied")
	}
	if !l.Allow("b") {
		t.Fatal("keys must be independent")
	}
	c.advance(500 * time.Millisecond) // +1 token
	if l.Blocked("a") || !l.Allow("a") || l.Allow("a") {
		t.Fatal("expected exactly one token after 500ms")
	}
	c.advance(time.Hour)
	for i := range 3 {
		if !l.Allow("a") {
			t.Fatalf("refill must cap at burst; request %d denied", i)
		}
	}
	if l.Allow("a") {
		t.Fatal("refill exceeded burst")
	}
}

func TestSweepDropsIdleKeys(t *testing.T) {
	c := &clock{t: time.Unix(1_700_000_000, 0)}
	l := New(1, 1, c.now)
	l.Allow("idle")
	l.Allow("busy")
	if l.Len() != 2 {
		t.Fatalf("Len = %d", l.Len())
	}
	c.advance(2 * time.Minute)
	l.Allow("busy") // triggers the sweep; "idle" is full again and dropped
	if l.Len() != 1 {
		t.Fatalf("Len after sweep = %d, want 1", l.Len())
	}
}

func TestConcurrentUse(t *testing.T) {
	l := New(0.0001, 50, nil)
	var wg sync.WaitGroup
	var mu sync.Mutex
	allowed := 0
	for range 200 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if l.Allow("k") {
				mu.Lock()
				allowed++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if allowed != 50 {
		t.Fatalf("allowed = %d, want exactly the burst of 50", allowed)
	}
}
