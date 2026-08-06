package main

import (
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

func ttlFor(name, domain string) int {
	if name == dnsKey(domain) {
		return rootTTL
	}
	return entryTTL
}
