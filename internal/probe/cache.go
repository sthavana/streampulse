package probe

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// cacheInfo is what a response says about its own freshness.
//
// It exists to answer the question a frozen manifest cannot answer on its own:
// *which layer* broke. A packager that has stopped producing and a CDN edge
// serving a stale cached copy look identical from here -- same bytes, poll
// after poll -- and they are different teams' problems. A stale edge is still
// a genuine viewer-facing fault, since players hitting that edge see the same
// frozen stream, so the answer is not to bypass the cache but to say which one
// it is.
type cacheInfo struct {
	// Age is the RFC 9111 Age header: how long ago the origin generated this
	// response, as reported by the caches in between. It is the reliable
	// signal here; the hit/miss headers are vendor-specific.
	Age    time.Duration
	HasAge bool
	// Hit is a normalised "HIT", "STALE", "MISS", or "" when nothing said.
	//
	// STALE is worth separating from HIT: nginx's UPDATING and STALE states
	// mean the edge knows its copy is past its TTL and is serving it anyway
	// while it revalidates. On a frozen manifest that is close to a diagnosis.
	Hit string
}

// hitHeaders are the vendor spellings of the same idea. X-Cached is Unified
// Streaming's, and is one letter away from the common one.
var hitHeaders = []string{"X-Cache", "X-Cached", "CF-Cache-Status", "X-Cache-Status", "X-Cache-Remote"}

func readCache(h http.Header) cacheInfo {
	var c cacheInfo
	if v := strings.TrimSpace(h.Get("Age")); v != "" {
		if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
			c.Age, c.HasAge = time.Duration(secs)*time.Second, true
		}
	}
	for _, name := range hitHeaders {
		if hit := normaliseHit(h.Get(name)); hit != "" {
			c.Hit = hit
			break
		}
	}
	return c
}

// normaliseHit reads one vendor's cache-status header. Only the last
// comma-separated element is considered: a CDN chain lists the shield first
// and the edge nearest the client last (Fastly's "HIT, MISS"), and it is the
// edge we were actually served by.
func normaliseHit(v string) string {
	if v == "" {
		return ""
	}
	parts := strings.Split(v, ",")
	last := strings.ToLower(strings.TrimSpace(parts[len(parts)-1]))
	// Order matters. "miss" is tested before "hit" because a miss is rarely
	// spelled with "hit" in it while a node name sometimes is, and the stale
	// states are tested before both because they are the interesting answer.
	switch {
	case strings.Contains(last, "stale"), strings.Contains(last, "updating"):
		return "STALE"
	case strings.Contains(last, "miss"), strings.Contains(last, "expired"),
		strings.Contains(last, "bypass"):
		return "MISS"
	case strings.Contains(last, "hit"), strings.Contains(last, "revalidated"):
		return "HIT"
	}
	return ""
}

func (c cacheInfo) ageSuffix() string {
	if !c.HasAge {
		return ""
	}
	return ", Age " + ftoa(c.Age.Seconds()) + "s"
}

// describe renders the cache headers for a finding message, or "" when the
// response said nothing about caching.
func (c cacheInfo) describe() string {
	var parts []string
	if c.HasAge {
		parts = append(parts, "Age "+ftoa(c.Age.Seconds())+"s")
	}
	if c.Hit != "" {
		parts = append(parts, c.Hit)
	}
	if len(parts) == 0 {
		return ""
	}
	return " [cache: " + strings.Join(parts, ", ") + "]"
}

// explain attributes a fault to a layer, given how long the manifest has been
// unchanged.
//
// The reasoning is only as strong as the Age header, and it is deliberately
// worded as evidence rather than a verdict. If the response we just read is
// younger than the freeze, the origin generated this frozen manifest a moment
// ago and the packager is the thing at fault. If it is at least as old as the
// freeze, one stale cached object could account for everything we have seen,
// and the origin may be perfectly healthy behind it.
func (c cacheInfo) explain(unchangedFor time.Duration) string {
	// The edge saying outright that it is serving past its TTL settles it,
	// with or without an Age header.
	if c.Hit == "STALE" {
		return " [cache: STALE" + c.ageSuffix() +
			" -- the edge is serving expired content; the packager may be fine]"
	}
	if !c.HasAge {
		if c.Hit == "HIT" {
			return " [cache: HIT, no Age header -- may be a stale edge rather than the packager]"
		}
		return c.describe()
	}
	base := " [cache: Age " + ftoa(c.Age.Seconds()) + "s"
	if c.Hit != "" {
		base += ", " + c.Hit
	}
	if c.Age >= unchangedFor {
		return base + " -- old enough to account for the freeze; check the origin before the packager]"
	}
	return base + " -- fresher than the freeze, so the origin is serving it]"
}
