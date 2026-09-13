// Package limit provides the two gates that stand between incoming requests and
// the browser:
//
//  1. Group coalesces concurrent requests for the same URL into one render, so a
//     crawler burst on a single page does not occupy every render slot with
//     duplicate work.
//  2. Semaphore caps how many renders run at once, which is what actually bounds
//     CPU and memory.
//
// Both are small enough to keep in the standard library. Nothing here spawns
// goroutines of its own.
package limit

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

// ErrQueueTimeout means the caller waited for a render slot longer than the
// configured queue timeout. It maps to HTTP 503 with a Retry-After header.
var ErrQueueTimeout = errors.New("timed out waiting for a render slot")

// Semaphore is a counting semaphore backed by a buffered channel.
type Semaphore struct {
	slots    chan struct{}
	inFlight atomic.Int64
	waiting  atomic.Int64
}

// NewSemaphore creates a semaphore permitting n concurrent holders.
func NewSemaphore(n int) *Semaphore {
	if n < 1 {
		n = 1
	}
	return &Semaphore{slots: make(chan struct{}, n)}
}

// Acquire blocks until a slot is free, the wait timeout elapses, or ctx is
// cancelled — whichever happens first. Honouring ctx matters: when a crawler
// hangs up while queued there is no point rendering for it, and releasing the
// wait immediately keeps the queue moving.
//
// The returned release function is safe to call exactly once and must always be
// deferred by the caller.
func (s *Semaphore) Acquire(ctx context.Context, wait time.Duration) (release func(), err error) {
	timer := time.NewTimer(wait)
	defer timer.Stop()

	s.waiting.Add(1)
	defer s.waiting.Add(-1)

	select {
	case s.slots <- struct{}{}:
		s.inFlight.Add(1)
		var once sync.Once
		return func() {
			once.Do(func() {
				s.inFlight.Add(-1)
				<-s.slots
			})
		}, nil
	case <-timer.C:
		return nil, ErrQueueTimeout
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Capacity returns the configured maximum.
func (s *Semaphore) Capacity() int { return cap(s.slots) }

// InFlight returns how many renders hold a slot right now.
func (s *Semaphore) InFlight() int64 { return s.inFlight.Load() }

// Waiting returns how many callers are queued for a slot.
func (s *Semaphore) Waiting() int64 { return s.waiting.Load() }

// Group deduplicates concurrent work by key. It is a trimmed equivalent of
// golang.org/x/sync/singleflight, inlined to keep the dependency list at two.
//
// The leader executes fn on its own goroutine. Because an http.Handler
// goroutine is not terminated when the client disconnects, a render started by
// a leader that goes away still completes and still populates the cache and its
// waiters — the "orphan complete" behaviour we want for a crawler cache.
type Group struct {
	mu    sync.Mutex
	calls map[string]*call
}

type call struct {
	wg      sync.WaitGroup
	val     any
	err     error
	waiters int
}

// Do runs fn for key unless a call for that key is already in flight, in which
// case it waits for and returns that call's result. shared reports whether the
// result came from another caller's execution.
func (g *Group) Do(key string, fn func() (any, error)) (val any, shared bool, err error) {
	g.mu.Lock()
	if g.calls == nil {
		g.calls = make(map[string]*call)
	}
	if c, ok := g.calls[key]; ok {
		c.waiters++
		g.mu.Unlock()
		c.wg.Wait()
		return c.val, true, c.err
	}

	c := new(call)
	c.wg.Add(1)
	g.calls[key] = c
	g.mu.Unlock()

	// A panic inside fn must not leave waiters blocked on wg forever, so the
	// bookkeeping is unwound before the panic propagates to the caller's
	// recovery middleware.
	defer func() {
		if r := recover(); r != nil {
			g.finish(key, c)
			panic(r)
		}
	}()

	c.val, c.err = fn()
	g.finish(key, c)
	return c.val, false, c.err
}

func (g *Group) finish(key string, c *call) {
	g.mu.Lock()
	if g.calls[key] == c {
		delete(g.calls, key)
	}
	g.mu.Unlock()
	c.wg.Done()
}

// InFlight returns the number of distinct keys currently being computed.
func (g *Group) InFlight() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.calls)
}
