package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MysticRyuujin/enrscout/internal/query"
)

func TestWriteErrDoesNotExposeInternalDetails(t *testing.T) {
	rr := httptest.NewRecorder()
	writeErr(rr, errors.New("s3 secret path /var/run/private"))
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rr.Code)
	}
	if strings.Contains(rr.Body.String(), "private") || !strings.Contains(rr.Body.String(), "internal server error") {
		t.Fatalf("unsafe error response: %s", rr.Body.String())
	}
}

func TestSecurityHeaders(t *testing.T) {
	h := securityHeaders(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))
	for _, name := range []string{"Content-Security-Policy", "Permissions-Policy", "Referrer-Policy", "X-Content-Type-Options", "X-Frame-Options"} {
		if rr.Header().Get(name) == "" {
			t.Errorf("missing %s", name)
		}
	}
}

func TestValidateNodeQuery(t *testing.T) {
	known := map[string]bool{"mainnet": true}
	if err := validateNodeQuery(url.Values{"network": {"mainnet"}, "protocol": {"v5"}, "order": {"asc"}, "fork": {"current"}}, known); err != nil {
		t.Fatalf("valid query rejected: %v", err)
	}
	if err := validateNodeQuery(url.Values{"fork": {"all"}}, known); err != nil {
		t.Fatalf("explicit all-fork query rejected: %v", err)
	}
	for _, q := range []url.Values{
		{"network": {"unknown"}},
		{"protocol": {"v6"}},
		{"sort": {"random()"}},
		{"order": {"sideways"}},
		{"fork": {"ancient"}},
		{"ip": {strings.Repeat("1", 65)}},
		{"q": {strings.Repeat("x", 1025)}},
	} {
		if err := validateNodeQuery(q, known); err == nil {
			t.Errorf("invalid query accepted: %v", q)
		}
	}
}

func TestParseIntParam(t *testing.T) {
	if got, err := parseIntParam("", 100, 1, 1000); err != nil || got != 100 {
		t.Fatalf("default = %d, %v", got, err)
	}
	for _, value := range []string{"nope", "0", "1001", "-1"} {
		if _, err := parseIntParam(value, 100, 1, 1000); err == nil {
			t.Errorf("invalid integer %q accepted", value)
		}
	}
}

func TestValidateNetworks(t *testing.T) {
	if err := validateNetworks([]string{"mainnet", "hoodi", "sepolia"}); err != nil {
		t.Fatalf("valid networks rejected: %v", err)
	}
	for _, networks := range [][]string{nil, {"mainnet", "mainnet"}, {"../mainnet"}, {"devnet.local"}, {"mainet"}, {"devnet-1"}} {
		if err := validateNetworks(networks); err == nil {
			t.Errorf("invalid networks accepted: %v", networks)
		}
	}
}

func TestValidateCORSOrigin(t *testing.T) {
	for _, origin := range []string{"", "*", "https://explorer.example.org", "http://localhost:8081"} {
		if err := validateCORSOrigin(origin); err != nil {
			t.Errorf("valid origin %q rejected: %v", origin, err)
		}
	}
	for _, origin := range []string{"javascript:alert(1)", "https://example.org/path", "https://user@example.org", "https://example.org\r\nX-Evil: yes"} {
		if err := validateCORSOrigin(origin); err == nil {
			t.Errorf("invalid origin %q accepted", origin)
		}
	}
}

func TestCORSDisabledByDefault(t *testing.T) {
	h := withCORS("", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))
	if rr.Header().Get("Access-Control-Allow-Origin") != "" || rr.Header().Get("Access-Control-Allow-Methods") != "" {
		t.Fatalf("CORS headers emitted while disabled: %v", rr.Header())
	}
}

func TestAgeSinceClampsClockSkew(t *testing.T) {
	if got := ageSince(time.Now().Add(time.Minute)); got != 0 {
		t.Fatalf("future snapshot age = %s, want 0", got)
	}
}

func TestMapETagUsesResponseContent(t *testing.T) {
	body := []byte(`{"type":"FeatureCollection","features":[]}`)
	etag := mapETag("devnet", body)
	if got := mapETag("devnet", append([]byte(nil), body...)); got != etag {
		t.Fatalf("same response body produced different ETags: %q != %q", got, etag)
	}
	if got := mapETag("devnet", append(body, '\n')); got == etag {
		t.Fatalf("different response body produced the same ETag %q", etag)
	}
	for _, header := range []string{etag, "W/" + etag, `"other", ` + etag, "*"} {
		if !etagMatches(header, etag) {
			t.Errorf("If-None-Match %q did not match %q", header, etag)
		}
	}
	if etagMatches(`"other"`, etag) {
		t.Fatal("unrelated ETag matched")
	}
	if strings.ContainsAny(mapETag("devnet-compact", body), "\x00\r\n") {
		t.Fatal("ETag contains an HTTP header control character")
	}
}

func TestMapCacheControlDoesNotCrossForkTransition(t *testing.T) {
	at := time.Unix(1700000000, 0)
	tests := []struct {
		name string
		next time.Time
		want string
	}{
		{"more than stale window", at.Add(361 * time.Second), "public, max-age=300, stale-while-revalidate=60"},
		{"inside stale window", at.Add(330 * time.Second), "public, max-age=300"},
		{"inside normal lifetime", at.Add(120 * time.Second), "public, max-age=120"},
		{"inside one minute", at.Add(30 * time.Second), "public, max-age=30"},
		{"at boundary", at, "public, max-age=0"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := mapCacheControl(at, test.next); got != test.want {
				t.Fatalf("Cache-Control = %q, want %q", got, test.want)
			}
		})
	}
}

func TestStatsLRUIsBounded(t *testing.T) {
	cache := newLRU[query.Stats](2)
	for _, key := range []string{"a", "b", "c"} {
		if _, err := cache.load(context.Background(), key, func() (query.Stats, error) { return query.Stats{ForkEvaluatedAt: key}, nil }); err != nil {
			t.Fatal(err)
		}
	}
	if len(cache.items) != 2 || cache.items["a"] != nil {
		t.Fatalf("LRU items = %v, want b/c only", cache.items)
	}
}

func TestCompactMapUsesTupleAndIDPrefix(t *testing.T) {
	got := compactMap([]query.Point{{
		ID: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", Lon: 12.5, Lat: -7.25,
		Client: "Geth", Country: "US", City: "Chicago", Subdivision: "IL", Layer: "el", Hosting: true, IPv6: false, Verified: true, AccuracyKM: 20,
	}})
	points, ok := got["points"].([][12]any)
	if !ok || len(points) != 1 {
		t.Fatalf("points = %#v", got["points"])
	}
	want := [12]any{"0123456789abcdef", 12.5, -7.25, "Geth", "US", "Chicago", "el", 1, 0, 1, 20, "IL"}
	if points[0] != want {
		t.Fatalf("point = %#v, want %#v", points[0], want)
	}
}

// Rollovers make every in-flight request miss the same key at once, and the connection pool is small
// enough that a handful of concurrent aggregates delay everything else.
func TestLRUDoesNotRunOneKeysLoaderTwice(t *testing.T) {
	cache := newLRU[int](4)
	var runs atomic.Int32
	release := make(chan struct{})
	var started, done sync.WaitGroup
	results := make([]int, 8)
	for i := range results {
		started.Add(1)
		done.Add(1)
		go func(slot int) {
			defer done.Done()
			started.Done()
			got, err := cache.load(context.Background(), "k", func() (int, error) {
				runs.Add(1)
				<-release
				return 42, nil
			})
			if err != nil {
				t.Errorf("load: %v", err)
			}
			results[slot] = got
		}(i)
	}
	started.Wait()
	close(release)
	done.Wait()

	if got := runs.Load(); got != 1 {
		t.Fatalf("loader ran %d times for one key, want 1", got)
	}
	for slot, got := range results {
		if got != 42 {
			t.Fatalf("waiter %d got %d, want 42", slot, got)
		}
	}
}

func TestLRUDoesNotCacheErrors(t *testing.T) {
	cache := newLRU[int](4)
	boom := errors.New("boom")
	if _, err := cache.load(context.Background(), "k", func() (int, error) { return 0, boom }); !errors.Is(err, boom) {
		t.Fatalf("first load err = %v", err)
	}
	got, err := cache.load(context.Background(), "k", func() (int, error) { return 7, nil })
	if err != nil || got != 7 {
		t.Fatalf("second load = %d, %v; want 7, nil", got, err)
	}
}

func TestMapCacheDoesNotCacheErrors(t *testing.T) {
	c := &mapCache{entries: map[string]*mapEntry{}}
	refresh := time.Unix(1700000000, 0)
	ctx := context.Background()
	calls := 0
	if _, err := c.load(ctx, "mainnet", refresh, func(context.Context) ([]byte, error) {
		calls++
		return nil, errors.New("transient")
	}); err == nil {
		t.Fatal("loader error was not returned")
	}
	body, err := c.load(ctx, "mainnet", refresh, func(context.Context) ([]byte, error) {
		calls++
		return []byte("map"), nil
	})
	if err != nil || string(body) != "map" {
		t.Fatalf("retry after error = %q, %v; an error must not be pinned until the next generation", body, err)
	}
	body, err = c.load(ctx, "mainnet", refresh, func(context.Context) ([]byte, error) {
		calls++
		return nil, errors.New("must not run: success is cached")
	})
	if err != nil || string(body) != "map" || calls != 2 {
		t.Fatalf("cached success = %q, %v, calls = %d", body, err, calls)
	}
}

func TestMapCacheSharesInFlightErrorWithWaiters(t *testing.T) {
	c := &mapCache{entries: map[string]*mapEntry{}}
	refresh := time.Unix(1700000000, 0)
	started := make(chan struct{})
	release := make(chan struct{})
	loaderErr := make(chan error, 1)
	go func() {
		_, err := c.load(context.Background(), "mainnet", refresh, func(context.Context) ([]byte, error) {
			close(started)
			<-release
			return nil, errors.New("shared failure")
		})
		loaderErr <- err
	}()
	<-started
	waiterErr := make(chan error, 1)
	go func() {
		_, err := c.load(context.Background(), "mainnet", refresh, func(context.Context) ([]byte, error) {
			return nil, errors.New("waiter ran its own loader")
		})
		waiterErr <- err
	}()
	time.Sleep(100 * time.Millisecond)
	close(release)
	for _, ch := range []chan error{loaderErr, waiterErr} {
		if err := <-ch; err == nil || err.Error() != "shared failure" {
			t.Fatalf("err = %v, want the coalesced attempt's shared failure", err)
		}
	}
}
