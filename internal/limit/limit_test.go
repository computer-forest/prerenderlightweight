package limit

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSemaphoreBoundsConcurrency(t *testing.T) {
	const capacity = 3
	sem := NewSemaphore(capacity)

	var current, peak atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 25; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, err := sem.Acquire(context.Background(), 2*time.Second)
			if err != nil {
				t.Errorf("acquire: %v", err)
				return
			}
			defer release()

			n := current.Add(1)
			for {
				p := peak.Load()
				if n <= p || peak.CompareAndSwap(p, n) {
					break
				}
			}
			time.Sleep(5 * time.Millisecond)
			current.Add(-1)
		}()
	}
	wg.Wait()

	if peak.Load() > capacity {
		t.Errorf("peak concurrency %d exceeded capacity %d", peak.Load(), capacity)
	}
	if sem.InFlight() != 0 {
		t.Errorf("in-flight = %d after all releases, want 0", sem.InFlight())
	}
}

func TestSemaphoreQueueTimeout(t *testing.T) {
	sem := NewSemaphore(1)
	release, err := sem.Acquire(context.Background(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	if _, err := sem.Acquire(context.Background(), 50*time.Millisecond); !errors.Is(err, ErrQueueTimeout) {
		t.Errorf("err = %v, want ErrQueueTimeout", err)
	}
}

func TestSemaphoreRespectsCancelledContext(t *testing.T) {
	sem := NewSemaphore(1)
	release, _ := sem.Acquire(context.Background(), time.Second)
	defer release()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	if _, err := sem.Acquire(ctx, 10*time.Second); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("waited %s after cancellation, expected immediate return", elapsed)
	}
}

func TestGroupCoalesces(t *testing.T) {
	var g Group
	var calls atomic.Int64
	release := make(chan struct{})

	var wg sync.WaitGroup
	results := make([]string, 20)
	sharedFlags := make([]bool, 20)

	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			v, shared, err := g.Do("same-url", func() (any, error) {
				calls.Add(1)
				<-release
				return "rendered", nil
			})
			if err != nil {
				t.Errorf("Do: %v", err)
				return
			}
			results[i], _ = v.(string)
			sharedFlags[i] = shared
		}(i)
	}

	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	if calls.Load() != 1 {
		t.Errorf("fn ran %d times, want 1", calls.Load())
	}
	leaders := 0
	for i := range results {
		if results[i] != "rendered" {
			t.Errorf("result[%d] = %q, want %q", i, results[i], "rendered")
		}
		if !sharedFlags[i] {
			leaders++
		}
	}
	if leaders != 1 {
		t.Errorf("%d callers reported as leader, want 1", leaders)
	}
	if g.InFlight() != 0 {
		t.Errorf("in-flight keys = %d after completion, want 0", g.InFlight())
	}
}

func TestGroupDifferentKeysRunIndependently(t *testing.T) {
	var g Group
	var calls atomic.Int64
	var wg sync.WaitGroup

	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _, _ = g.Do(string(rune('a'+i)), func() (any, error) {
				calls.Add(1)
				return i, nil
			})
		}(i)
	}
	wg.Wait()

	if calls.Load() != 5 {
		t.Errorf("fn ran %d times, want 5", calls.Load())
	}
}

// A panic in the leader must not leave waiters blocked forever.
func TestGroupUnblocksWaitersOnPanic(t *testing.T) {
	var g Group
	done := make(chan struct{})

	go func() {
		defer func() {
			_ = recover()
			close(done)
		}()
		_, _, _ = g.Do("boom", func() (any, error) {
			panic("render exploded")
		})
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("leader goroutine never returned after panic")
	}

	if g.InFlight() != 0 {
		t.Errorf("in-flight keys = %d after panic, want 0", g.InFlight())
	}
}

func TestGroupPropagatesError(t *testing.T) {
	var g Group
	sentinel := errors.New("render failed")
	_, _, err := g.Do("k", func() (any, error) { return nil, sentinel })
	if !errors.Is(err, sentinel) {
		t.Errorf("err = %v, want %v", err, sentinel)
	}
}
