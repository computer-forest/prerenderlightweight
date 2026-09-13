// Package cache stores rendered HTML in Redis.
//
// HTML is stored as a plain Redis string under a readable key so that the cache
// can be inspected directly:
//
//	GET  "prerender:html:https://www.sbisecurities.in/about-us"
//	TTL  "prerender:html:https://www.sbisecurities.in/about-us"
//	HGETALL "prerender:meta:https://www.sbisecurities.in/about-us"
//
// The HTML is deliberately not compressed. Compression would roughly halve
// memory use but would make every cached value unreadable from redis-cli, which
// is a requirement here.
package cache

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	"prerender/internal/config"
	"prerender/internal/urlutil"
)

// ErrMiss is returned by Get when the key is absent.
var ErrMiss = errors.New("cache miss")

// Cache is a thin wrapper over a Redis client.
type Cache struct {
	rdb     *redis.Client
	prefix  string
	ttl     time.Duration
	timeout time.Duration
	enabled bool
	meta    bool
}

// Meta describes a cached render. It is stored as a Redis hash alongside the
// HTML purely for operator visibility; nothing in the request path reads it.
type Meta struct {
	RenderedAt string
	DurationMS int64
	Status     int
	Bytes      int
	Tier       string
}

// New builds a cache backed by the configured Redis instance. It does not
// connect eagerly; use Ping to verify reachability.
func New(cfg *config.Config) *Cache {
	rdb := redis.NewClient(&redis.Options{
		Addr:         cfg.RedisAddr,
		Password:     cfg.RedisPassword,
		DB:           cfg.RedisDB,
		DialTimeout:  cfg.RedisTimeout,
		ReadTimeout:  cfg.RedisTimeout,
		WriteTimeout: cfg.RedisTimeout,
		PoolSize:     cfg.MaxConcurrentRenders + 4,
	})
	return &Cache{
		rdb:     rdb,
		prefix:  cfg.RedisPrefix,
		ttl:     cfg.CacheTTL,
		timeout: cfg.RedisTimeout,
		enabled: cfg.CacheEnabled,
		meta:    cfg.CacheMeta,
	}
}

// Enabled reports whether caching is switched on.
func (c *Cache) Enabled() bool { return c.enabled }

// TTL returns the configured entry lifetime.
func (c *Cache) TTL() time.Duration { return c.ttl }

// Key returns the Redis key a normalized URL maps to.
func (c *Cache) Key(normalizedURL string) string {
	return urlutil.CacheKey(c.prefix, normalizedURL)
}

// Get returns cached HTML. A missing key returns ErrMiss, which callers treat
// as a normal cache miss. Any other error is a genuine Redis failure: callers
// should log it and fall through to rendering rather than failing the request.
func (c *Cache) Get(ctx context.Context, normalizedURL string) (string, error) {
	if !c.enabled {
		return "", ErrMiss
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	html, err := c.rdb.Get(ctx, c.Key(normalizedURL)).Result()
	switch {
	case errors.Is(err, redis.Nil):
		return "", ErrMiss
	case err != nil:
		return "", fmt.Errorf("redis get %s: %w", c.Key(normalizedURL), err)
	}
	return html, nil
}

// Set stores rendered HTML with the configured TTL, plus the metadata hash when
// enabled. Both writes go out in one pipeline, so a store costs a single round
// trip.
func (c *Cache) Set(ctx context.Context, normalizedURL, html string, meta Meta) error {
	if !c.enabled {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	key := c.Key(normalizedURL)
	pipe := c.rdb.Pipeline()
	pipe.Set(ctx, key, html, c.ttl)

	if c.meta {
		metaKey := urlutil.MetaKey(c.prefix, normalizedURL)
		pipe.HSet(ctx, metaKey, map[string]any{
			"url":         normalizedURL,
			"rendered_at": meta.RenderedAt,
			"duration_ms": meta.DurationMS,
			"status":      meta.Status,
			"bytes":       meta.Bytes,
			"ready_tier":  meta.Tier,
		})
		pipe.Expire(ctx, metaKey, c.ttl)
	}

	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("redis set %s: %w", key, err)
	}
	return nil
}

// Delete removes a cached entry and its metadata. Used by the admin purge
// endpoint.
func (c *Cache) Delete(ctx context.Context, normalizedURL string) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	n, err := c.rdb.Del(ctx,
		c.Key(normalizedURL),
		urlutil.MetaKey(c.prefix, normalizedURL),
	).Result()
	if err != nil {
		return 0, fmt.Errorf("redis del %s: %w", c.Key(normalizedURL), err)
	}
	return n, nil
}

// Ping verifies Redis is reachable. Used by the readiness endpoint.
func (c *Cache) Ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	if err := c.rdb.Ping(ctx).Err(); err != nil {
		return fmt.Errorf("redis ping %s: %w", c.rdb.Options().Addr, err)
	}
	return nil
}

// Close releases the connection pool.
func (c *Cache) Close() error { return c.rdb.Close() }
