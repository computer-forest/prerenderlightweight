package render

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chromedp/chromedp"

	"prerender/internal/config"
)

// Browser owns a single long-lived Chromium process.
//
// Starting a browser process costs 300-800ms; opening a tab inside a running
// one costs 20-50ms. So the process is started once at boot and every render
// gets its own tab, which is torn down when the render finishes. That single
// decision is most of the difference between a fast prerenderer and a slow one.
//
// Chromium leaks memory slowly across thousands of renders, so the process is
// recycled after a render budget or when a health probe fails. Recycling takes
// the write lock, which by definition waits for every in-flight render (each
// holds the read lock) to finish first.
type Browser struct {
	cfg *config.Config
	log *slog.Logger

	mu            sync.RWMutex
	allocCancel   context.CancelFunc
	browserCtx    context.Context
	browserCancel context.CancelFunc
	rootCtx       context.Context
	started       bool
	closed        bool

	renders    atomic.Int64 // renders served by the current process
	generation atomic.Int64 // incremented on every (re)start
	total      atomic.Int64 // renders served since boot
	recycling  atomic.Bool
}

// NewBrowser prepares a browser manager. Call Start before use.
func NewBrowser(cfg *config.Config, log *slog.Logger) *Browser {
	return &Browser{cfg: cfg, log: log}
}

// Start launches the Chromium process. rootCtx should be the application
// context: cancelling it tears the browser down.
func (b *Browser) Start(rootCtx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rootCtx = rootCtx
	return b.startLocked()
}

func (b *Browser) startLocked() error {
	if b.closed {
		return fmt.Errorf("browser manager is closed")
	}

	opts := append([]chromedp.ExecAllocatorOption{}, chromedp.DefaultExecAllocatorOptions[:]...)
	opts = append(opts,
		// The "new" headless mode; the legacy one diverges from real Chrome.
		chromedp.Flag("headless", "new"),
		// Required when running as root or in a container without user
		// namespaces. Safe here only because ALLOWED_HOSTS restricts what can
		// be loaded.
		chromedp.NoSandbox,
		chromedp.Flag("disable-gpu", true),
		chromedp.Flag("disable-software-rasterizer", true),
		chromedp.Flag("mute-audio", true),
		chromedp.Flag("hide-scrollbars", true),
		chromedp.Flag("disable-notifications", true),
		chromedp.Flag("disable-translate", true),
		chromedp.Flag("window-size", "1280,1024"),
	)
	if b.cfg.BlockResources {
		// Belt and braces alongside Network.setBlockedURLs: this stops the
		// image decode path inside Blink entirely.
		opts = append(opts, chromedp.Flag("blink-settings", "imagesEnabled=false"))
	}
	if b.cfg.ChromePath != "" {
		opts = append(opts, chromedp.ExecPath(b.cfg.ChromePath))
	}

	allocCtx, allocCancel := chromedp.NewExecAllocator(b.rootCtx, opts...)

	// chromedp's own diagnostics are routed into structured logging at debug
	// level so they never write unformatted lines to stdout.
	browserCtx, browserCancel := chromedp.NewContext(allocCtx,
		chromedp.WithLogf(func(f string, a ...any) {
			b.log.Debug("chromedp", "msg", fmt.Sprintf(f, a...))
		}),
		chromedp.WithErrorf(func(f string, a ...any) {
			b.log.Warn("chromedp_error", "msg", fmt.Sprintf(f, a...))
		}),
	)

	// chromedp.Run on a fresh context is what actually spawns the process, and
	// the exec allocator binds the OS process's lifetime to whatever context
	// that first Run call receives. Wrapping it in context.WithTimeout(browserCtx, ...)
	// would mean the deferred cancel kills the browser the moment startLocked
	// returns, so the 30s startup bound is enforced with a timer instead,
	// leaving browserCtx itself untimed for the process's whole lifetime.
	startErr := make(chan error, 1)
	go func() { startErr <- chromedp.Run(browserCtx) }()

	var err error
	select {
	case err = <-startErr:
	case <-time.After(30 * time.Second):
		err = fmt.Errorf("timed out after 30s")
	}
	if err != nil {
		browserCancel()
		allocCancel()
		return fmt.Errorf("start chromium (CHROME_PATH=%q): %w", b.cfg.ChromePath, err)
	}

	b.allocCancel = allocCancel
	b.browserCtx = browserCtx
	b.browserCancel = browserCancel
	b.started = true
	b.renders.Store(0)
	gen := b.generation.Add(1)

	b.log.Info("browser.started", "generation", gen, "chrome_path", b.cfg.ChromePath)
	return nil
}

// Session is one tab, held for the duration of a single render.
type Session struct {
	Ctx context.Context

	browser *Browser
	cancel  context.CancelFunc
	once    sync.Once
}

// Acquire opens a tab on the running browser. The caller must always defer
// Close on the returned session; that single deferred call is what guarantees
// tabs cannot leak, on any code path including panics.
func (b *Browser) Acquire() (*Session, error) {
	b.mu.RLock()
	if !b.started || b.closed {
		b.mu.RUnlock()
		return nil, fmt.Errorf("browser is not running")
	}
	if err := b.browserCtx.Err(); err != nil {
		b.mu.RUnlock()
		return nil, fmt.Errorf("browser context is dead: %w", err)
	}

	tabCtx, cancel := chromedp.NewContext(b.browserCtx)
	return &Session{Ctx: tabCtx, browser: b, cancel: cancel}, nil
}

// Close tears down the tab and releases the browser read lock. Safe to call
// more than once.
func (s *Session) Close() {
	s.once.Do(func() {
		s.cancel()
		s.browser.mu.RUnlock()
		s.browser.afterRender()
	})
}

// afterRender applies the render budget. Recycling happens on its own goroutine
// because it needs the write lock, which this caller cannot take while it still
// holds the read lock.
func (b *Browser) afterRender() {
	b.total.Add(1)
	n := b.renders.Add(1)
	if b.cfg.BrowserMaxRenders <= 0 || n < int64(b.cfg.BrowserMaxRenders) {
		return
	}
	if !b.recycling.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer b.recycling.Store(false)
		b.Recycle(fmt.Sprintf("render budget reached (%d renders)", n))
	}()
}

// Recycle replaces the Chromium process. It blocks until every in-flight render
// has finished, then restarts. A failed restart leaves the manager unhealthy
// rather than killing the process: Health will report false and readiness will
// fail, but the service stays up and keeps retrying.
func (b *Browser) Recycle(reason string) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.closed {
		return
	}
	served := b.renders.Load()
	b.log.Info("browser.recycle", "reason", reason, "renders_served", served)

	b.stopLocked()
	if err := b.startLocked(); err != nil {
		b.log.Error("browser.recycle_failed", "reason", reason, "error", err.Error())
		b.started = false
	}
}

func (b *Browser) stopLocked() {
	if b.browserCancel != nil {
		b.browserCancel()
	}
	if b.allocCancel != nil {
		// Waits for the Chromium process to actually exit, so the next start
		// cannot race with a zombie.
		b.allocCancel()
	}
	b.browserCtx = nil
	b.browserCancel = nil
	b.allocCancel = nil
	b.started = false
}

// Health runs a trivial navigation in a throwaway tab. Used by /readyz.
func (b *Browser) Health(ctx context.Context) error {
	sess, err := b.Acquire()
	if err != nil {
		return err
	}
	defer sess.Close()

	probeCtx, cancel := context.WithTimeout(sess.Ctx, 10*time.Second)
	defer cancel()

	var ok bool
	if err := chromedp.Run(probeCtx,
		chromedp.Navigate("about:blank"),
		chromedp.Evaluate(`true`, &ok),
	); err != nil {
		return fmt.Errorf("browser health probe: %w", err)
	}
	if !ok {
		return fmt.Errorf("browser health probe returned false")
	}
	return nil
}

// Stats reports browser counters for the metrics endpoint.
func (b *Browser) Stats() (generation, rendersThisProcess, rendersTotal int64, running bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.generation.Load(), b.renders.Load(), b.total.Load(), b.started && !b.closed
}

// Close shuts the browser down for good. It waits for in-flight renders.
func (b *Browser) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.closed = true
	b.stopLocked()
	b.log.Info("browser.closed", "renders_total", b.total.Load())
}
