package main

import (
	"fmt"
	"math"
	"strings"
	"time"
)

const (
	// Hash-named entries are content-addressed and never change, so they cache hard. The root is
	// replaced every cycle and bounds how long a client can hold a superseded tree.
	entryTTL = 3600
	rootTTL  = 300

	// dnsdisc clients re-check a tree root every 30 minutes by default.
	minPublishInterval = 30 * time.Minute
)

// An entry never changes under its name, so its TTL trades only DNS query volume against cache
// churn; the root TTL adds to how long a client can serve a superseded tree.
type recordTTLs struct {
	entry int
	root  int
}

var defaultTTLs = recordTTLs{entry: entryTTL, root: rootTTL}

func (t recordTTLs) forName(name, domain string) int {
	if name == dnsKey(domain) {
		return t.root
	}
	return t.entry
}

// A DNS TTL is a 31-bit count of whole seconds (RFC 2181 §8), and Cloudflare rejects a TTL above
// one day on zones below Enterprise.
func validateTTLs(entry, root time.Duration, cloudflare bool) (recordTTLs, error) {
	out := recordTTLs{}
	for _, f := range []struct {
		name string
		d    time.Duration
		dst  *int
	}{
		{"--entry-ttl", entry, &out.entry},
		{"--root-ttl", root, &out.root},
	} {
		if f.d < time.Second || f.d%time.Second != 0 {
			return out, fmt.Errorf("%s must be a whole number of seconds, at least 1s", f.name)
		}
		if f.d/time.Second > math.MaxInt32 {
			return out, fmt.Errorf("%s exceeds the 31-bit DNS TTL limit", f.name)
		}
		if cloudflare && (f.d < cloudflareMinTTL || f.d > cloudflareMaxTTL) {
			return out, fmt.Errorf("%s outside %s-%s is rejected by Cloudflare zones below Enterprise", f.name, cloudflareMinTTL, cloudflareMaxTTL)
		}
		*f.dst = int(f.d / time.Second)
	}
	return out, nil
}

// normalizeTXT undoes the presentation form a zone may return for content longer than a single
// 255-byte character-string, so an unchanged record is not seen as changed every cycle.
func normalizeTXT(content string) string {
	if !strings.HasPrefix(content, `"`) {
		return content
	}
	var b strings.Builder
	inQuote := false
	escaped := false
	for _, r := range content {
		switch {
		case escaped:
			b.WriteRune(r)
			escaped = false
		case r == '\\':
			escaped = true
		case r == '"':
			inQuote = !inQuote
		case inQuote:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// ToTXT emits uppercase base32 labels; a zone returns them lowercased, and comparing raw names would
// see every entry as both missing and stale.
func dnsKey(name string) string {
	return strings.ToLower(strings.TrimSuffix(name, "."))
}
