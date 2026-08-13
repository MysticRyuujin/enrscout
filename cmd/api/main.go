package main

import (
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/MysticRyuujin/enrscout/internal/buildinfo"
	"github.com/MysticRyuujin/enrscout/internal/debugsrv"
	"github.com/MysticRyuujin/enrscout/internal/devnetconfig"
	"github.com/MysticRyuujin/enrscout/internal/metricsrv"
	"github.com/MysticRyuujin/enrscout/internal/netconf"
	"github.com/MysticRyuujin/enrscout/internal/query"
	"github.com/MysticRyuujin/enrscout/internal/snapshot"
	"github.com/MysticRyuujin/enrscout/internal/store"
)

func main() {
	if err := run(); err != nil {
		slog.Error("api exited", "err", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		addr        = flag.String("addr", ":8080", "HTTP listen address")
		networksF   = flag.String("networks", "mainnet,hoodi,sepolia", "comma-separated networks to serve")
		devnetDir   = flag.String("devnet-dir", "", "register the custom devnet described by this config directory before serving")
		refresh     = flag.Duration("refresh", 60*time.Second, "snapshot refresh interval")
		maxAge      = flag.Duration("max-snapshot-age", 15*time.Minute, "/readyz fails when the loaded snapshot is older than this")
		tmpdir      = flag.String("tmpdir", "", "local dir for downloaded snapshots (empty = private per-process temporary directory)")
		prefix      = flag.String("prefix", "snapshots", "object key prefix")
		corsOrigin  = flag.String("cors-origin", "", "Access-Control-Allow-Origin response value (empty disables cross-origin response sharing; not an authorization control)")
		pprofAddr   = flag.String("pprof", "", "serve net/http/pprof on this address (empty = off)")
		metricsAddr = flag.String("metrics-addr", "127.0.0.1:9101", "serve Prometheus metrics on this private listener (empty = disabled)")
		data        = flag.String("data", "data", "filesystem snapshot dir (used when --s3-endpoint is empty)")
		s3Endpoint  = flag.String("s3-endpoint", "", "S3-compatible endpoint (host:port); empty uses filesystem")
		s3Bucket    = flag.String("s3-bucket", "enrscout", "S3 bucket")
		s3Region    = flag.String("s3-region", "us-east-1", "S3 region")
		s3SSL       = flag.Bool("s3-ssl", true, "use TLS for the S3 endpoint (set false only for a trusted local endpoint)")
	)
	flag.Parse()

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))
	if strings.TrimSpace(*devnetDir) != "" {
		cfg, err := devnetconfig.Load(*devnetDir)
		if err != nil {
			return fmt.Errorf("load devnet: %w", err)
		}
		if err := netconf.RegisterDevnet(cfg); err != nil {
			return fmt.Errorf("register devnet: %w", err)
		}
	}

	networks := splitCSV(*networksF)
	if err := validateNetworks(networks); err != nil {
		return fmt.Errorf("--networks: %w", err)
	}
	if *refresh <= 0 {
		return fmt.Errorf("--refresh must be positive, got %s", *refresh)
	}
	if *maxAge <= 0 {
		return fmt.Errorf("--max-snapshot-age must be positive, got %s", *maxAge)
	}
	if err := validateCORSOrigin(*corsOrigin); err != nil {
		return fmt.Errorf("--cors-origin: %w", err)
	}
	if err := debugsrv.Start(*pprofAddr); err != nil {
		return err
	}
	if err := metricsrv.Start(*metricsAddr, "api"); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(ctx, store.S3Config{
		Endpoint: *s3Endpoint, Region: *s3Region, Bucket: *s3Bucket,
		AccessKey: os.Getenv("S3_ACCESS_KEY"), SecretKey: os.Getenv("S3_SECRET_KEY"), UseSSL: *s3SSL,
	}, *data)
	if err != nil {
		return err
	}

	eng, err := query.New(st, networks, *tmpdir, *prefix)
	if err != nil {
		return err
	}
	defer eng.Close()

	initErr := eng.Refresh(ctx)
	recordRefresh(eng, initErr)
	if initErr != nil {
		slog.Warn("initial snapshot refresh failed; serving empty until snapshots exist", "err", initErr)
	}
	refreshDone := make(chan struct{})
	go func() {
		defer close(refreshDone)
		refreshLoop(ctx, eng, *refresh)
	}()

	srv := &http.Server{
		Addr:              *addr,
		Handler:           routes(eng, *corsOrigin, *maxAge, networks),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()

	slog.Info("api listening", "addr", *addr, "store", st.Backend(), "networks", networks)
	serveErr := srv.ListenAndServe()
	if serveErr == http.ErrServerClosed {
		serveErr = nil
	}
	// Drain and stop the refresh loop before eng.Close on the error path too.
	stop()
	<-shutdownDone
	<-refreshDone
	return serveErr
}

func refreshLoop(ctx context.Context, eng *query.Engine, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			err := eng.Refresh(ctx)
			recordRefresh(eng, err)
			if err != nil {
				slog.Warn("snapshot refresh failed", "err", err)
				continue
			}
			slog.Info("snapshots refreshed", "nodes", eng.Loaded())
		}
	}
}

func routes(eng *query.Engine, cors string, maxAge time.Duration, networks []string) http.Handler {
	mux := http.NewServeMux()
	known := make(map[string]bool, len(networks))
	for _, n := range networks {
		known[n] = true
	}

	mux.HandleFunc("GET /livez", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
	})

	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		age, ok := snapshotAge(eng)
		if !ok {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "no snapshot"})
			return
		}
		if age > maxAge {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "stale", "age_seconds": int(age.Seconds())})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "age_seconds": int(age.Seconds())})
	})

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		body := map[string]any{"status": "ok", "nodes": eng.Loaded()}
		if g := eng.GeneratedAt(); !g.IsZero() {
			body["generated_at"] = g.UTC().Format(time.RFC3339)
			body["age_seconds"] = int(ageSince(g).Seconds())
		}
		writeJSON(w, http.StatusOK, body)
	})

	mux.HandleFunc("GET /api/v1/meta", instrument("/api/v1/meta", func(w http.ResponseWriter, r *http.Request) {
		run := eng.RunMetadata()
		revision, sourceURL := run.SourceRevision, run.SourceURL
		if revision == "" {
			revision = buildinfo.Revision
		}
		if sourceURL == "" {
			sourceURL = buildinfo.SourceURL
		}
		body := map[string]any{
			"nodes": eng.Loaded(), "networks": networks, "schema_version": eng.SchemaVersion(),
			"source_revision": revision, "source_url": sourceURL, "crawler_id": eng.CrawlerID(),
		}
		if run.RunID != "" {
			body["run_id"] = run.RunID
		}
		if run.MethodologyVersion != "" {
			body["methodology_version"] = run.MethodologyVersion
		}
		if run.MethodologyID != "" {
			body["methodology_id"] = run.MethodologyID
		}
		if run.ConfigSHA256 != "" {
			body["config_sha256"] = run.ConfigSHA256
		}
		if run.ImageDigest != "" {
			body["image_digest"] = run.ImageDigest
		}
		if !run.CrawlerStartedAt.IsZero() {
			body["crawler_started_at"] = run.CrawlerStartedAt.UTC().Format(time.RFC3339Nano)
		}
		if !run.MethodologyStartedAt.IsZero() {
			body["methodology_started_at"] = run.MethodologyStartedAt.UTC().Format(time.RFC3339Nano)
		}
		if g := eng.GeneratedAt(); !g.IsZero() {
			body["generated_at"] = g.UTC().Format(time.RFC3339)
			body["age_seconds"] = int(ageSince(g).Seconds())
		}
		writeJSON(w, http.StatusOK, body)
	}))

	mux.HandleFunc("GET /api/v1/nodes", instrument("/api/v1/nodes", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if err := validateNodeQuery(q, known); err != nil {
			writeClientErr(w, err)
			return
		}
		limit, err := parseIntParam(q.Get("limit"), 100, 1, 1000)
		if err != nil {
			writeClientErr(w, err)
			return
		}
		offset, err := parseIntParam(q.Get("offset"), 0, 0, 1_000_000)
		if err != nil {
			writeClientErr(w, err)
			return
		}
		forkStatus := q.Get("fork")
		if forkStatus == "" {
			forkStatus = "current"
		}
		f := query.Filter{
			Network: q.Get("network"), Client: q.Get("client"), Country: q.Get("country"),
			Layer: q.Get("layer"), Protocol: q.Get("protocol"), IPStack: q.Get("ipstack"), Hosting: q.Get("hosting"),
			Dialable: q.Get("dialable"), ForkStatus: forkStatus,
			Membership: q.Get("membership"),
			IP:         q.Get("ip"), Q: q.Get("q"), Sort: q.Get("sort"), Order: q.Get("order"),
			Limit: limit, Offset: offset,
		}
		res, err := eng.Nodes(r.Context(), f)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, res)
	}))

	mux.HandleFunc("GET /api/v1/nodes/{key}", instrument("/api/v1/nodes/{key}", func(w http.ResponseWriter, r *http.Request) {
		key := r.PathValue("key")
		if key == "" || len(key) > 8<<10 {
			writeClientErr(w, errors.New("invalid node key"))
			return
		}
		n, err := eng.NodeByKey(r.Context(), key)
		if err != nil {
			writeErr(w, err)
			return
		}
		if n == nil {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
			return
		}
		writeJSON(w, http.StatusOK, n)
	}))

	stats := newLRU[query.Stats](128)
	versions := newLRU[cachedVersions](256)
	mux.HandleFunc("GET /api/v1/stats", instrument("/api/v1/stats", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if err := validateNetwork(q.Get("network"), known); err != nil {
			writeClientErr(w, err)
			return
		}
		if len(q.Get("client")) > 128 {
			writeClientErr(w, errors.New("client filter too long"))
			return
		}
		// Trimmed once here so the cache key and the exact-client comparison cannot disagree: keying
		// on a trimmed value while querying an untrimmed one lets " Geth " cache an empty breakdown
		// under Geth's key.
		client := strings.TrimSpace(q.Get("client"))
		membership := q.Get("membership")
		if membership != "" && membership != "verified" && membership != "claimed" && membership != "all" {
			writeClientErr(w, errors.New("membership must be verified, claimed, or all"))
			return
		}
		key := q.Get("network")
		at := time.Now()
		era, _, err := forkEra(at, key)
		if err != nil {
			writeErr(w, err)
			return
		}
		result, err := loadStats(r.Context(), eng, stats, versions, key, client, membership, era, at)
		if err != nil {
			writeErr(w, err)
			return
		}
		body, err := json.Marshal(result)
		if err != nil {
			writeErr(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))

	maps := &mapCache{entries: map[string]*mapEntry{}}
	mux.HandleFunc("GET /api/v1/map", instrument("/api/v1/map", func(w http.ResponseWriter, r *http.Request) {
		network := r.URL.Query().Get("network")
		if err := validateNetwork(network, known); err != nil {
			writeClientErr(w, err)
			return
		}
		format := r.URL.Query().Get("format")
		if format != "" && format != "compact" {
			writeClientErr(w, errors.New("format must be compact when set"))
			return
		}
		membership := r.URL.Query().Get("membership")
		if membership != "" && membership != "verified" && membership != "claimed" && membership != "all" {
			writeClientErr(w, errors.New("membership must be verified, claimed, or all"))
			return
		}
		// Served networks only (arbitrary keys grow the map unboundedly); the pre-query
		// refresh key keeps a mid-request refresh from caching stale points as current.
		at := time.Now()
		era, nextTransition, err := forkEra(at, network)
		if err != nil {
			writeErr(w, err)
			return
		}
		refreshedAt := eng.LastRefresh()
		cacheKey := network + "\x00" + format + "\x00" + membership + "\x00" + era
		body, err := maps.load(r.Context(), cacheKey, refreshedAt, func(ctx context.Context) ([]byte, error) {
			pts, total, err := eng.MapPointsForMembershipAt(ctx, network, membership, at)
			if err != nil {
				return nil, err
			}
			if format == "compact" {
				return json.Marshal(withMapCoverage(compactMap(pts), len(pts), total))
			}
			return json.Marshal(withMapCoverage(geoJSON(pts), len(pts), total))
		})
		if err != nil {
			writeErr(w, err)
			return
		}
		etagKey := network
		if format == "compact" {
			etagKey += "-compact"
		}
		etagKey += "-" + membership
		etagKey += "-" + era
		etag := mapETag(etagKey, body)
		w.Header().Set("Cache-Control", mapCacheControl(at, nextTransition))
		w.Header().Set("ETag", etag)
		if etagMatches(r.Header.Get("If-None-Match"), etag) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))

	return securityHeaders(withCORS(cors, mux))
}

func mapETag(network string, body []byte) string {
	h := sha256.New()
	_, _ = h.Write([]byte(network))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write(body)
	return fmt.Sprintf(`"map-%x"`, h.Sum(nil))
}

func forkEra(at time.Time, network string) (string, time.Time, error) {
	if network == "" {
		return netconf.ForkEraTokenAt(at)
	}
	return netconf.ForkEraTokenAt(at, network)
}

func mapCacheControl(at, nextTransition time.Time) string {
	maxAge := 300
	if !nextTransition.IsZero() {
		remaining := nextTransition.Sub(at)
		if remaining <= 0 {
			maxAge = 0
		} else if seconds := int(remaining / time.Second); seconds < maxAge {
			maxAge = seconds
		}
	}
	value := fmt.Sprintf("public, max-age=%d", maxAge)
	if nextTransition.IsZero() || nextTransition.Sub(at) >= time.Duration(maxAge+60)*time.Second {
		value += ", stale-while-revalidate=60"
	}
	return value
}

// If-None-Match uses weak comparison for GET/HEAD and may contain a list of
// validators. Our response validator is strong, but accepting its W/ form is
// required by RFC 9110 weak comparison semantics.
func etagMatches(header, etag string) bool {
	want := strings.TrimPrefix(etag, "W/")
	for part := range strings.SplitSeq(header, ",") {
		part = strings.TrimSpace(part)
		if part == "*" || strings.TrimPrefix(part, "W/") == want {
			return true
		}
	}
	return false
}

const stableGenerationAttempts = 3

type cachedVersions struct {
	byVersion  map[string]int
	generation time.Time
}

// loadStats serves the client-independent aggregate and the per-client version breakdown from
// separate cache entries so an arbitrary client filter cannot re-run every aggregate. They are two
// queries under two publication barriers, so a refresh can land between them; pairing on the
// generation each ran against detects that and one retry lands both on the newer table.
func loadStats(ctx context.Context, eng *query.Engine, aggregates *lru[query.Stats], versions *lru[cachedVersions], network, client, membership, era string, at time.Time) (query.Stats, error) {
	minute := strconv.FormatInt(at.Truncate(time.Minute).Unix(), 10)
	normalized := strings.ToLower(client)
	// The aggregate's key space is server-derived and small, so its loads really are shared and must
	// survive one caller disconnecting. The version breakdown's key embeds the caller-supplied client
	// filter, so those loads almost never have waiters; detaching them would let unique filters leave
	// abandoned queries occupying the small connection pool, so they stay bound to the request.
	shared, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	for attempt := 0; ; attempt++ {
		generation := membership + "\x00" + eng.LastRefresh().UTC().Format(time.RFC3339Nano) + "\x00" + era + "\x00" + minute
		result, err := aggregates.load(ctx, network+"\x00"+generation, func() (query.Stats, error) {
			return eng.StatsForMembershipAt(shared, network, membership, at)
		})
		if err != nil || client == "" {
			return result, err
		}
		cached, err := versions.load(ctx, network+"\x00"+normalized+"\x00"+generation, func() (cachedVersions, error) {
			byVersion, ranAgainst, err := eng.VersionsForMembershipAt(ctx, network, client, membership, at)
			return cachedVersions{byVersion: byVersion, generation: ranAgainst}, err
		})
		if err != nil {
			return result, err
		}
		if cached.generation.Equal(result.Generation) {
			// result aliases the cached entry's maps, so only the local copy's field may be replaced.
			result.ByVersion = cached.byVersion
			return result, nil
		}
		// Refreshes kept landing between the two queries, which an arbitrarily small --refresh
		// allows. Serving the aggregate with an empty breakdown is honest; merging halves known to
		// come from different generations is not.
		if attempt+1 >= stableGenerationAttempts {
			return result, nil
		}
	}
}

type lruEntry[V any] struct {
	key   string
	value V
}

type inflightLoad[V any] struct {
	ready chan struct{}
	value V
	err   error
}

type lru[V any] struct {
	mu       sync.Mutex
	capacity int
	items    map[string]*list.Element
	order    *list.List
	pending  map[string]*inflightLoad[V]
}

func newLRU[V any](capacity int) *lru[V] {
	return &lru[V]{
		capacity: capacity, items: make(map[string]*list.Element, capacity), order: list.New(),
		pending: map[string]*inflightLoad[V]{},
	}
}

// load coalesces concurrent misses for one key onto a single loader run. Without that, every request
// arriving at a generation or minute rollover runs the full aggregate itself, and the connection pool
// is small enough that a handful of them delay everything else. Errors are shared but never cached.
func (c *lru[V]) load(ctx context.Context, key string, loader func() (V, error)) (V, error) {
	for {
		c.mu.Lock()
		if element := c.items[key]; element != nil {
			c.order.MoveToFront(element)
			value := element.Value.(lruEntry[V]).value
			c.mu.Unlock()
			return value, nil
		}
		if pending := c.pending[key]; pending != nil {
			c.mu.Unlock()
			select {
			case <-ctx.Done():
				var zero V
				return zero, ctx.Err()
			case <-pending.ready:
			}
			// A cancelled load says nothing about the query, only that the request driving it went
			// away, so a still-connected waiter runs it again on its own context instead of
			// inheriting a 500. Real failures are shared, which is what avoids a stampede.
			if pending.err != nil && !errors.Is(pending.err, context.Canceled) && !errors.Is(pending.err, context.DeadlineExceeded) {
				return pending.value, pending.err
			}
			continue
		}
		pending := &inflightLoad[V]{ready: make(chan struct{})}
		c.pending[key] = pending
		c.mu.Unlock()

		pending.value, pending.err = loader()
		c.mu.Lock()
		delete(c.pending, key)
		if pending.err == nil {
			element := c.order.PushFront(lruEntry[V]{key: key, value: pending.value})
			c.items[key] = element
			if c.order.Len() > c.capacity {
				oldest := c.order.Back()
				delete(c.items, oldest.Value.(lruEntry[V]).key)
				c.order.Remove(oldest)
			}
		}
		c.mu.Unlock()
		close(pending.ready)
		return pending.value, pending.err
	}
}

type mapEntry struct {
	at    time.Time
	body  []byte
	err   error
	ready chan struct{}
}

type mapCache struct {
	mu      sync.Mutex
	entries map[string]*mapEntry
}

func (c *mapCache) load(ctx context.Context, network string, refresh time.Time, fn func(context.Context) ([]byte, error)) ([]byte, error) {
	c.mu.Lock()
	// Serving same-or-newer entries keeps waiters that raced a refresh from clobbering a fresher in-flight loader.
	if e, ok := c.entries[network]; ok && !e.at.Before(refresh) {
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-e.ready:
			return e.body, e.err
		}
	}
	e := &mapEntry{at: refresh, ready: make(chan struct{})}
	c.entries[network] = e
	c.mu.Unlock()

	// One waiter's cancellation must not abort the query shared by all coalesced waiters.
	lctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	e.body, e.err = fn(lctx)
	cancel()
	c.mu.Lock()
	// An error is shared with this attempt's coalesced waiters but never cached: LastRefresh
	// only advances on a table swap, so a cached error would be served until the next NEW
	// generation - potentially forever if the crawler stops publishing.
	if e.err != nil && c.entries[network] == e {
		delete(c.entries, network)
	}
	c.mu.Unlock()
	close(e.ready)
	return e.body, e.err
}

func snapshotAge(eng *query.Engine) (time.Duration, bool) {
	g := eng.GeneratedAt()
	if g.IsZero() {
		return 0, false
	}
	return ageSince(g), true
}

func ageSince(g time.Time) time.Duration {
	age := time.Since(g)
	if age < 0 {
		age = 0
	}
	return age
}

func withCORS(origin string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if origin != "" {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
		w.Header().Set("Permissions-Policy", "camera=(), geolocation=(), microphone=()")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}

func geoJSON(pts []query.Point) map[string]any {
	features := make([]map[string]any, 0, len(pts))
	for _, p := range pts {
		features = append(features, map[string]any{
			"type":     "Feature",
			"geometry": map[string]any{"type": "Point", "coordinates": []float64{p.Lon, p.Lat}},
			"properties": map[string]any{
				"id": p.ID, "client": p.Client, "network": p.Network, "country": p.Country, "city": p.City, "subdivision": p.Subdivision,
				"layer": p.Layer, "hosting": p.Hosting, "ipv6": p.IPv6, "verified": p.Verified, "accuracy_km": p.AccuracyKM,
			},
		})
	}
	return map[string]any{"type": "FeatureCollection", "features": features}
}

func compactMap(pts []query.Point) map[string]any {
	points := make([][12]any, 0, len(pts))
	for _, p := range pts {
		id := p.ID
		if len(id) > 16 {
			id = id[:16]
		}
		points = append(points, [12]any{id, p.Lon, p.Lat, p.Client, p.Country, p.City, p.Layer, boolInt(p.Hosting), boolInt(p.IPv6), boolInt(p.Verified), int(p.AccuracyKM), p.Subdivision})
	}
	return map[string]any{"points": points}
}

// withMapCoverage discloses a bounded response instead of letting a caller read the rendered subset
// as the whole population. GeoJSON permits foreign members, so both encodings carry the same keys.
func withMapCoverage(body map[string]any, returned, total int) map[string]any {
	body["returned"] = returned
	body["total"] = total
	body["truncated"] = returned < total
	return body
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// 499 is the nginx convention for a client that closed the request; only the handlers'
// contexts derive from the request, so context.Canceled here is never a server fault.
func writeErr(w http.ResponseWriter, err error) {
	if errors.Is(err, context.Canceled) {
		slog.Debug("request canceled by client", "err", err)
		writeJSON(w, 499, map[string]any{"error": "client closed request"})
		return
	}
	slog.Error("request failed", "err", err)
	writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal server error"})
}

func writeClientErr(w http.ResponseWriter, err error) {
	writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
}

func parseIntParam(s string, def, min, max int) (int, error) {
	if s == "" {
		return def, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < min || n > max {
		return 0, fmt.Errorf("integer parameter must be between %d and %d", min, max)
	}
	return n, nil
}

var nodeQueryLimits = map[string]int{"client": 128, "country": 16, "ip": 64, "q": 1024}

var nodeQueryAllowed = map[string]map[string]bool{
	"layer":      {"": true, "el": true, "cl": true},
	"protocol":   {"": true, "v4": true, "v5": true},
	"ipstack":    {"": true, "ipv4": true, "ipv6": true, "dual": true},
	"hosting":    {"": true, "yes": true, "no": true},
	"dialable":   {"": true, "yes": true, "no": true},
	"fork":       {"": true, "current": true, "stale": true, "all": true},
	"membership": {"": true, "verified": true, "claimed": true, "all": true},
}

func validateNodeQuery(q map[string][]string, known map[string]bool) error {
	if err := validateNetwork(first(q["network"]), known); err != nil {
		return err
	}
	for name, limit := range nodeQueryLimits {
		if len(first(q[name])) > limit {
			return fmt.Errorf("%s filter too long", name)
		}
	}
	for name, values := range nodeQueryAllowed {
		if value := first(q[name]); !values[value] {
			return fmt.Errorf("invalid %s parameter", name)
		}
	}
	if !query.ValidSort(first(q["sort"])) {
		return errors.New("invalid sort parameter")
	}
	if !query.ValidOrder(first(q["order"])) {
		return errors.New("invalid order parameter")
	}
	return nil
}

func validateNetwork(network string, known map[string]bool) error {
	if network != "" && !known[network] {
		return fmt.Errorf("unknown network %q", network)
	}
	return nil
}

func first(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func validateNetworks(networks []string) error {
	if len(networks) == 0 {
		return errors.New("must list at least one network")
	}
	seen := make(map[string]bool, len(networks))
	for _, network := range networks {
		if !snapshot.ValidComponent(network) {
			return fmt.Errorf("invalid network name %q", network)
		}
		if _, err := netconf.Get(network); err != nil {
			return err
		}
		if seen[network] {
			return fmt.Errorf("duplicate network %q", network)
		}
		seen[network] = true
	}
	return nil
}

func validateCORSOrigin(origin string) error {
	if origin == "" || origin == "*" {
		return nil
	}
	u, err := url.Parse(origin)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("must be empty, *, or an http(s) origin without a path")
	}
	return nil
}
