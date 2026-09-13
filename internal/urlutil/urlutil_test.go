package urlutil

import "testing"

func TestExtract(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
		bad  bool
	}{
		{"plain", "/https://www.sbisecurities.in", "https://www.sbisecurities.in", false},
		{"with path", "/https://www.sbisecurities.in/about-us", "https://www.sbisecurities.in/about-us", false},
		{"with query", "/https://www.sbisecurities.in/products?id=7&x=1", "https://www.sbisecurities.in/products?id=7&x=1", false},
		{"collapsed slash", "/https:/www.sbisecurities.in/about-us", "https://www.sbisecurities.in/about-us", false},
		{"http scheme", "/http://www.sbisecurities.in", "http://www.sbisecurities.in", false},
		{"percent encoded", "/https%3A%2F%2Fwww.sbisecurities.in/about", "https://www.sbisecurities.in/about", false},
		{"fragment survives extraction", "/https://www.sbisecurities.in/about#team", "https://www.sbisecurities.in/about#team", false},
		{"root only", "/", "", true},
		{"empty", "", "", true},
		{"no scheme", "/www.sbisecurities.in", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Extract(tc.in)
			if tc.bad {
				if err == nil {
					t.Fatalf("Extract(%q) = %q, want error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Extract(%q) error: %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("Extract(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestNormalize(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"lowercases host", "https://WWW.SBISecurities.IN/About-Us", "https://www.sbisecurities.in/About-Us"},
		{"keeps path case", "https://www.sbisecurities.in/About-Us", "https://www.sbisecurities.in/About-Us"},
		{"drops default https port", "https://www.sbisecurities.in:443/x", "https://www.sbisecurities.in/x"},
		{"drops default http port", "http://www.sbisecurities.in:80/x", "http://www.sbisecurities.in/x"},
		{"keeps custom port", "https://www.sbisecurities.in:8443/x", "https://www.sbisecurities.in:8443/x"},
		{"adds root path", "https://www.sbisecurities.in", "https://www.sbisecurities.in/"},
		{"trims trailing slash", "https://www.sbisecurities.in/about/", "https://www.sbisecurities.in/about"},
		{"drops fragment", "https://www.sbisecurities.in/about#team", "https://www.sbisecurities.in/about"},
		{"sorts query params", "https://www.sbisecurities.in/p?b=2&a=1", "https://www.sbisecurities.in/p?a=1&b=2"},
		{"sorts repeated values", "https://www.sbisecurities.in/p?a=2&a=1", "https://www.sbisecurities.in/p?a=1&a=2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Normalize(tc.in)
			if err != nil {
				t.Fatalf("Normalize(%q) error: %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("Normalize(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestNormalizeRejectsNonHTTP(t *testing.T) {
	for _, in := range []string{"file:///etc/passwd", "ftp://example.com", "javascript:alert(1)"} {
		if got, err := Normalize(in); err == nil {
			t.Errorf("Normalize(%q) = %q, want error", in, got)
		}
	}
}

// Equivalent URLs must share a key; different URLs must never collide.
func TestCacheKeyDeterminism(t *testing.T) {
	equivalent := [][2]string{
		{"https://www.sbisecurities.in/p?a=1&b=2", "https://www.sbisecurities.in/p?b=2&a=1"},
		{"https://www.sbisecurities.in/about#top", "https://www.sbisecurities.in/about"},
		{"https://WWW.sbisecurities.in/about", "https://www.sbisecurities.in/about/"},
	}
	for _, pair := range equivalent {
		a, err := Normalize(pair[0])
		if err != nil {
			t.Fatal(err)
		}
		b, err := Normalize(pair[1])
		if err != nil {
			t.Fatal(err)
		}
		if CacheKey("prerender:html:", a) != CacheKey("prerender:html:", b) {
			t.Errorf("expected same key for %q and %q", pair[0], pair[1])
		}
	}

	distinct := []string{
		"https://www.sbisecurities.in/about",
		"https://www.sbisecurities.in/About",
		"https://www.sbisecurities.in/about?a=1",
		"https://www.sbisecurities.in/about?a=2",
		"http://www.sbisecurities.in/about",
		"https://sbisecurities.in/about",
	}
	seen := map[string]string{}
	for _, u := range distinct {
		n, err := Normalize(u)
		if err != nil {
			t.Fatal(err)
		}
		k := CacheKey("prerender:html:", n)
		if prev, dup := seen[k]; dup {
			t.Errorf("key collision: %q and %q both map to %q", prev, u, k)
		}
		seen[k] = u
	}
}

// The key must remain readable so it can be inspected from redis-cli.
func TestCacheKeyIsReadable(t *testing.T) {
	n, err := Normalize("https://www.sbisecurities.in/about-us")
	if err != nil {
		t.Fatal(err)
	}
	want := "prerender:html:https://www.sbisecurities.in/about-us"
	if got := CacheKey("prerender:html:", n); got != want {
		t.Errorf("CacheKey = %q, want %q", got, want)
	}
	wantMeta := "prerender:meta:https://www.sbisecurities.in/about-us"
	if got := MetaKey("prerender:html:", n); got != wantMeta {
		t.Errorf("MetaKey = %q, want %q", got, wantMeta)
	}
}

func TestHost(t *testing.T) {
	n, _ := Normalize("https://WWW.sbisecurities.in:8443/x")
	if got := Host(n); got != "www.sbisecurities.in" {
		t.Errorf("Host = %q, want www.sbisecurities.in", got)
	}
}
