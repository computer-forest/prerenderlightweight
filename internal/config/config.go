// Package config loads service configuration from environment variables.
//
// Every setting has a sensible default, so the service starts with zero
// configuration. Invalid values are a fatal startup error: it is better to fail
// loudly at boot than to serve traffic with a misparsed timeout.
package config

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the fully resolved runtime configuration.
type Config struct {
	// Server
	Port            string
	ReadTimeout     time.Duration
	WriteTimeout    time.Duration
	IdleTimeout     time.Duration
	ShutdownGrace   time.Duration

	// Security
	AllowedHosts map[string]bool

	// Redis
	RedisAddr     string
	RedisPassword string
	RedisDB       int
	RedisPrefix   string
	RedisTimeout  time.Duration
	CacheTTL      time.Duration
	CacheEnabled  bool
	CacheMeta     bool

	// Concurrency
	MaxConcurrentRenders int
	QueueWaitTimeout     time.Duration

	// Browser
	ChromePath        string
	RenderTimeout     time.Duration
	NavigationTimeout time.Duration
	MinWaitMS         int
	MaxWaitMS         int
	NetworkIdleMS     int
	PollIntervalMS    int
	StabilityChecks   int
	BlockResources    bool
	BlockedURLPatterns []string
	BlockedPathPatterns []string
	BrowserMaxRenders int
	MinHTMLBytes      int

	// Logging
	LogLevel string
}

// Load reads configuration from the environment. If an .env file is present in
// the working directory (or at the path given by ENV_FILE) its values are
// loaded first, without overriding variables already set in the real
// environment.
func Load() (*Config, error) {
	if err := loadDotEnv(getenv("ENV_FILE", ".env")); err != nil {
		return nil, err
	}

	var errs []string
	fail := func(format string, a ...any) { errs = append(errs, fmt.Sprintf(format, a...)) }

	c := &Config{
		Port:          getenv("PORT", "8080"),
		RedisAddr:     getenv("REDIS_ADDR", "127.0.0.1:6379"),
		RedisPassword: getenv("REDIS_PASSWORD", ""),
		RedisPrefix:   getenv("REDIS_KEY_PREFIX", "prerender:html:"),
		ChromePath:    getenv("CHROME_PATH", ""),
		LogLevel:      strings.ToLower(getenv("LOG_LEVEL", "info")),
	}

	dur := func(key, def string) time.Duration {
		v, err := time.ParseDuration(getenv(key, def))
		if err != nil {
			fail("%s: invalid duration %q", key, os.Getenv(key))
			return 0
		}
		return v
	}
	num := func(key, def string) int {
		v, err := strconv.Atoi(getenv(key, def))
		if err != nil {
			fail("%s: invalid integer %q", key, os.Getenv(key))
			return 0
		}
		return v
	}

	c.ReadTimeout = dur("READ_TIMEOUT", "15s")
	c.WriteTimeout = dur("WRITE_TIMEOUT", "120s")
	c.IdleTimeout = dur("IDLE_TIMEOUT", "60s")
	c.ShutdownGrace = dur("SHUTDOWN_GRACE_PERIOD", "60s")

	c.RedisDB = num("REDIS_DB", "0")
	c.RedisTimeout = dur("REDIS_TIMEOUT", "2s")
	c.CacheTTL = dur("CACHE_TTL", "6h")
	c.CacheEnabled = boolean("CACHE_ENABLED", true)
	c.CacheMeta = boolean("CACHE_META", true)

	c.MaxConcurrentRenders = num("MAX_CONCURRENT_RENDERS", "4")
	c.QueueWaitTimeout = dur("QUEUE_WAIT_TIMEOUT", "30s")

	c.RenderTimeout = dur("RENDER_TIMEOUT", "45s")
	c.NavigationTimeout = dur("NAVIGATION_TIMEOUT", "20s")
	c.MinWaitMS = num("MIN_WAIT_MS", "300")
	c.MaxWaitMS = num("MAX_WAIT_MS", "8000")
	c.NetworkIdleMS = num("NETWORK_IDLE_MS", "500")
	c.PollIntervalMS = num("POLL_INTERVAL_MS", "150")
	c.StabilityChecks = num("STABILITY_CHECKS", "3")
	c.BlockResources = boolean("BLOCK_RESOURCES", true)
	c.BrowserMaxRenders = num("BROWSER_MAX_RENDERS", "500")
	c.MinHTMLBytes = num("MIN_HTML_BYTES", "512")

	c.BlockedURLPatterns = splitList(getenv("BLOCKED_URL_PATTERNS", strings.Join(defaultBlockedPatterns, ",")))
	c.BlockedPathPatterns = splitList(getenv("BLOCKED_PATH_PATTERNS", ""))

	hosts := splitList(getenv("ALLOWED_HOSTS", ""))
	c.AllowedHosts = make(map[string]bool, len(hosts))
	for _, h := range hosts {
		c.AllowedHosts[strings.ToLower(h)] = true
	}
	if len(c.AllowedHosts) == 0 {
		fail("ALLOWED_HOSTS: must list at least one host; " +
			"without it this service is an open proxy that will fetch any URL on the internet")
	}

	// Sanity checks that catch genuinely dangerous combinations.
	if c.MaxConcurrentRenders < 1 {
		fail("MAX_CONCURRENT_RENDERS: must be >= 1")
	}
	if c.NavigationTimeout >= c.RenderTimeout {
		fail("NAVIGATION_TIMEOUT (%s) must be less than RENDER_TIMEOUT (%s)", c.NavigationTimeout, c.RenderTimeout)
	}
	if c.ShutdownGrace <= c.RenderTimeout {
		fail("SHUTDOWN_GRACE_PERIOD (%s) must exceed RENDER_TIMEOUT (%s), or shutdown kills live renders",
			c.ShutdownGrace, c.RenderTimeout)
	}
	if c.MaxWaitMS <= c.MinWaitMS {
		fail("MAX_WAIT_MS must be greater than MIN_WAIT_MS")
	}
	if c.StabilityChecks < 1 {
		fail("STABILITY_CHECKS: must be >= 1")
	}

	if len(errs) > 0 {
		return nil, fmt.Errorf("invalid configuration:\n  - %s", strings.Join(errs, "\n  - "))
	}
	return c, nil
}

// defaultBlockedPatterns drops payloads a crawler never needs. Images and fonts
// typically dominate page load time, so blocking them is the single largest
// rendering speedup available.
var defaultBlockedPatterns = []string{
	"*.png", "*.jpg", "*.jpeg", "*.gif", "*.webp", "*.avif", "*.ico", "*.bmp",
	"*.svg", "*.woff", "*.woff2", "*.ttf", "*.otf", "*.eot",
	"*.mp4", "*.webm", "*.mp3", "*.avi", "*.mov",
	"*google-analytics.com*", "*googletagmanager.com*", "*doubleclick.net*",
	"*facebook.net*", "*connect.facebook.com*", "*hotjar.com*",
	"*clarity.ms*", "*newrelic.com*", "*nr-data.net*",
	// Live chat widgets and social embeds: never useful in a crawler snapshot,
	// and the first things to hang if a deploy's egress can't reach them.
	"*.my.site.com*", "*platform.twitter.com*",
}

// Redacted returns the config as log-safe key/value pairs for the startup line.
func (c *Config) Redacted() []any {
	hosts := make([]string, 0, len(c.AllowedHosts))
	for h := range c.AllowedHosts {
		hosts = append(hosts, h)
	}
	return []any{
		"port", c.Port,
		"allowed_hosts", strings.Join(hosts, ","),
		"redis_addr", c.RedisAddr,
		"cache_enabled", c.CacheEnabled,
		"cache_ttl", c.CacheTTL.String(),
		"max_concurrent_renders", c.MaxConcurrentRenders,
		"render_timeout", c.RenderTimeout.String(),
		"navigation_timeout", c.NavigationTimeout.String(),
		"max_wait_ms", c.MaxWaitMS,
		"block_resources", c.BlockResources,
		"blocked_path_patterns", strings.Join(c.BlockedPathPatterns, ","),
		"browser_max_renders", c.BrowserMaxRenders,
		"log_level", c.LogLevel,
	}
}

func getenv(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func boolean(key string, def bool) bool {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// loadDotEnv is a minimal KEY=VALUE reader. It exists so the service needs no
// third-party dotenv dependency. A missing file is not an error.
func loadDotEnv(path string) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("open env file %s: %w", path, err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	line := 0
	for sc.Scan() {
		line++
		text := strings.TrimSpace(sc.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		text = strings.TrimPrefix(text, "export ")
		key, value, found := strings.Cut(text, "=")
		if !found {
			return fmt.Errorf("%s:%d: expected KEY=VALUE", path, line)
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if len(value) >= 2 {
			if (value[0] == '"' && value[len(value)-1] == '"') ||
				(value[0] == '\'' && value[len(value)-1] == '\'') {
				value = value[1 : len(value)-1]
			}
		}
		// Real environment variables win over the file.
		if _, exists := os.LookupEnv(key); !exists {
			if err := os.Setenv(key, value); err != nil {
				return fmt.Errorf("%s:%d: set %s: %w", path, line, key, err)
			}
		}
	}
	return sc.Err()
}
