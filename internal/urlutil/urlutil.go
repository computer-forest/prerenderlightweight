// Package urlutil extracts the target URL from an incoming prerender request
// and normalizes it into a deterministic, human-readable cache key.
//
// This package is deliberately independent of net/http and chromedp so its
// behaviour can be unit tested in isolation. It is the subtlest part of the
// service: Go's URL handling normalizes away the double slash in
// "/https://example.com", so extraction works on the raw request URI.
package urlutil

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
)

var (
	// ErrEmptyTarget means the request was "/" with nothing after it.
	ErrEmptyTarget = errors.New("no target URL supplied")
	// ErrBadScheme means the target was not http or https.
	ErrBadScheme = errors.New("target URL must use http or https")
	// ErrNoHost means the target URL had no host component.
	ErrNoHost = errors.New("target URL has no host")
)

// Extract pulls the target URL out of a raw request URI of the form
//
//	/https://www.sbisecurities.in/about-us?page=2
//
// requestURI must be the *raw* URI (http.Request.RequestURI), not
// http.Request.URL.Path. Go's net/url collapses "//" into "/" during parsing,
// which would turn "https://host" into "https:/host" before the handler ever
// sees it.
//
// Three encodings of the separator are accepted, because crawlers and proxies
// do not agree on how to write them:
//
//	/https://host/path      the normal form
//	/https:/host/path       after path cleaning collapsed the double slash
//	/https%3A%2F%2Fhost     fully percent-encoded
func Extract(requestURI string) (string, error) {
	raw := strings.TrimSpace(requestURI)
	raw = strings.TrimPrefix(raw, "/")
	if raw == "" {
		return "", ErrEmptyTarget
	}

	// Percent-encoded form: decode just enough to recover the scheme separator.
	if !strings.Contains(raw, "://") && strings.Contains(strings.ToLower(raw), "%3a%2f%2f") {
		decoded, err := url.PathUnescape(raw)
		if err != nil {
			return "", fmt.Errorf("percent-decode target: %w", err)
		}
		raw = decoded
	}

	// Repair a collapsed separator: "https:/host" -> "https://host".
	// Guard against "https:///host" by only repairing the single-slash case.
	for _, scheme := range []string{"http", "https"} {
		lower := strings.ToLower(raw)
		single := scheme + ":/"
		double := scheme + "://"
		if strings.HasPrefix(lower, single) && !strings.HasPrefix(lower, double) {
			raw = raw[:len(scheme)+2] + "/" + raw[len(scheme)+2:]
			break
		}
	}

	if !strings.Contains(raw, "://") {
		return "", ErrBadScheme
	}
	return raw, nil
}

// Normalize parses and canonicalizes a target URL so that URLs which address
// the same resource produce the same string, and URLs which address different
// resources never do.
//
// Rules:
//   - scheme and host are lowercased; default ports are dropped
//   - the path keeps its case (paths are case sensitive) and loses a trailing
//     slash, except for the root path
//   - query parameters are sorted by key, then by value, so ?a=1&b=2 and
//     ?b=2&a=1 share one cache entry
//   - the fragment is discarded: it is never transmitted to a server, so
//     /about#team and /about are the same request
func Normalize(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("parse target URL: %w", err)
	}

	u.Scheme = strings.ToLower(u.Scheme)
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", ErrBadScheme
	}

	host := strings.ToLower(u.Host)
	if host == "" {
		return "", ErrNoHost
	}
	host = strings.TrimSuffix(host, ".")
	switch {
	case u.Scheme == "http" && strings.HasSuffix(host, ":80"):
		host = strings.TrimSuffix(host, ":80")
	case u.Scheme == "https" && strings.HasSuffix(host, ":443"):
		host = strings.TrimSuffix(host, ":443")
	}
	u.Host = host

	if u.Path == "" {
		u.Path = "/"
	} else if len(u.Path) > 1 {
		u.Path = strings.TrimSuffix(u.Path, "/")
	}

	// Sort query parameters deterministically. url.Values.Encode already sorts
	// by key; it preserves the given order of repeated values, so sort those too.
	if u.RawQuery != "" {
		values, err := url.ParseQuery(u.RawQuery)
		if err != nil {
			return "", fmt.Errorf("parse query string: %w", err)
		}
		for _, v := range values {
			sort.Strings(v)
		}
		u.RawQuery = values.Encode()
	}

	u.Fragment = ""
	u.RawFragment = ""
	u.User = nil

	return u.String(), nil
}

// Host returns the lowercased hostname of a normalized URL, without the port.
// Used for the allowlist check.
func Host(normalizedURL string) string {
	u, err := url.Parse(normalizedURL)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

// CacheKey builds the Redis key for a normalized URL.
//
// The normalized URL is used verbatim rather than hashed. That makes the key
// deterministic by construction, guarantees two different URLs can never share
// a key, and — the reason it matters here — keeps the keyspace readable from
// redis-cli:
//
//	GET "prerender:html:https://www.sbisecurities.in/about-us"
//	SCAN 0 MATCH "prerender:html:*" COUNT 100
func CacheKey(prefix, normalizedURL string) string {
	return prefix + normalizedURL
}

// MetaKey returns the companion metadata key for a normalized URL.
func MetaKey(prefix, normalizedURL string) string {
	return strings.TrimSuffix(prefix, "html:") + "meta:" + normalizedURL
}

// MatchesAny reports whether target matches any of the given patterns,
// case-insensitively. A pattern without "*" is a plain substring match; with
// "*" it is a wildcard glob (e.g. "*/sso-page/*").
func MatchesAny(patterns []string, target string) bool {
	target = strings.ToLower(target)
	for _, p := range patterns {
		p = strings.ToLower(p)
		if !strings.Contains(p, "*") {
			if strings.Contains(target, p) {
				return true
			}
			continue
		}
		segments := strings.Split(p, "*")
		for i, s := range segments {
			segments[i] = regexp.QuoteMeta(s)
		}
		re, err := regexp.Compile(strings.Join(segments, ".*"))
		if err == nil && re.MatchString(target) {
			return true
		}
	}
	return false
}
