package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

const (
	cloudflareAPI      = "https://api.cloudflare.com/client/v4"
	cloudflareBatchMax = 200
	cloudflarePageSize = 100
	cloudflareTimeout  = 30 * time.Second
	cloudflareSettle   = 15 * time.Second
	maxCloudflareBody  = 8 << 20
	// Cloudflare accepts 60-86400s on zones below Enterprise; below that only the sentinel 1
	// ("automatic"), which is not a one-second TTL.
	cloudflareMinTTL = time.Minute
	cloudflareMaxTTL = 24 * time.Hour

	maxCloudflareTokenBytes = 1 << 10
)

type cloudflareDNS struct {
	http    *http.Client
	baseURL string
	zoneID  string
	token   string
	ttls    recordTTLs
	settle  time.Duration
}

func newCloudflareDNS(zoneID, token string, ttls recordTTLs) *cloudflareDNS {
	return &cloudflareDNS{
		http:    &http.Client{Timeout: cloudflareTimeout},
		baseURL: cloudflareAPI,
		zoneID:  zoneID,
		token:   token,
		ttls:    ttls,
		settle:  cloudflareSettle,
	}
}

type cfRecord struct {
	ID      string `json:"id,omitempty"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Content string `json:"content"`
	TTL     int    `json:"ttl"`
}

type cfEnvelope struct {
	Success bool            `json:"success"`
	Errors  []cfAPIError    `json:"errors"`
	Result  json.RawMessage `json:"result"`
	Info    struct {
		Page       int `json:"page"`
		TotalPages int `json:"total_pages"`
	} `json:"result_info"`
}

type cfAPIError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e cfAPIError) String() string { return fmt.Sprintf("%d %s", e.Code, e.Message) }

func (c *cloudflareDNS) do(ctx context.Context, method, path string, body any) (*cfEnvelope, error) {
	var reader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxCloudflareBody))
	if err != nil {
		return nil, err
	}
	var env cfEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("cloudflare %s %s: http %d: %w", method, path, resp.StatusCode, err)
	}
	if !env.Success {
		msgs := make([]string, 0, len(env.Errors))
		for _, e := range env.Errors {
			msgs = append(msgs, e.String())
		}
		// The token is never echoed, but an error body can carry zone details, so only the
		// structured error list is surfaced.
		return nil, fmt.Errorf("cloudflare %s %s: http %d: %s", method, path, resp.StatusCode, strings.Join(msgs, "; "))
	}
	return &env, nil
}

func (c *cloudflareDNS) existing(ctx context.Context, domain string) (map[string]cfRecord, error) {
	out := map[string]cfRecord{}
	suffix := "." + dnsKey(domain)
	for page := 1; ; page++ {
		path := fmt.Sprintf("/zones/%s/dns_records?type=TXT&per_page=%d&page=%d", c.zoneID, cloudflarePageSize, page)
		env, err := c.do(ctx, http.MethodGet, path, nil)
		if err != nil {
			return nil, err
		}
		var records []cfRecord
		if err := json.Unmarshal(env.Result, &records); err != nil {
			return nil, err
		}
		for _, r := range records {
			key := dnsKey(r.Name)
			if key == dnsKey(domain) || strings.HasSuffix(key, suffix) {
				r.Name = key
				r.Content = normalizeTXT(r.Content)
				out[key] = r
			}
		}
		if len(records) == 0 || env.Info.TotalPages <= page {
			return out, nil
		}
	}
}

type cfBatch struct {
	Deletes []cfRecord `json:"deletes,omitempty"`
	Puts    []cfRecord `json:"puts,omitempty"`
	Posts   []cfRecord `json:"posts,omitempty"`
}

func (b cfBatch) len() int { return len(b.Deletes) + len(b.Puts) + len(b.Posts) }

func (c *cloudflareDNS) submit(ctx context.Context, batch cfBatch) error {
	if batch.len() == 0 {
		return nil
	}
	_, err := c.do(ctx, http.MethodPost, "/zones/"+c.zoneID+"/dns_records/batch", batch)
	return err
}

// Sync reconciles the zone to want. Entries are written before the root so a resolver never follows
// a new root into a subtree that does not exist yet, and records are removed only once they are
// absent from both want and retain, so a client still holding the previous root can finish its walk.
func (c *cloudflareDNS) Sync(ctx context.Context, domain string, want, retain map[string]string) (int, error) {
	have, err := c.existing(ctx, domain)
	if err != nil {
		return 0, fmt.Errorf("list records: %w", err)
	}
	wanted := make(map[string]string, len(want))
	for name, content := range want {
		wanted[dnsKey(name)] = content
	}
	var entries, root, stale cfBatch
	for name, content := range wanted {
		current, exists := have[name]
		if exists && current.Content == content && current.TTL == c.ttls.forName(name, domain) {
			continue
		}
		record := cfRecord{Type: "TXT", Name: name, Content: content, TTL: c.ttls.forName(name, domain)}
		target := &entries
		if name == dnsKey(domain) {
			target = &root
		}
		if exists {
			record.ID = current.ID
			target.Puts = append(target.Puts, record)
			continue
		}
		target.Posts = append(target.Posts, record)
	}
	// A nil retain means nothing is known to have been published, so the zone may already be serving
	// a tree this process did not write. Pruning then would delete a live generation.
	if retain != nil {
		retained := make(map[string]struct{}, len(retain))
		for name := range retain {
			retained[dnsKey(name)] = struct{}{}
		}
		for name, current := range have {
			if _, keep := wanted[name]; keep {
				continue
			}
			if _, keep := retained[name]; keep {
				continue
			}
			stale.Deletes = append(stale.Deletes, cfRecord{ID: current.ID})
		}
	}

	changed := 0
	for _, chunk := range chunkBatch(entries) {
		if err := c.submit(ctx, chunk); err != nil {
			return changed, fmt.Errorf("write entries: %w", err)
		}
		changed += chunk.len()
	}
	// Ordering the API writes does not order propagation to the edge, so give new entries a moment to
	// become resolvable before the root starts pointing at them.
	if entries.len() > 0 && c.settle > 0 {
		select {
		case <-ctx.Done():
			return changed, ctx.Err()
		case <-time.After(c.settle):
		}
	}
	if err := c.submit(ctx, root); err != nil {
		return changed, fmt.Errorf("write root: %w", err)
	}
	changed += root.len()
	// A failed prune leaves records the current root simply does not reference, so it must not fail
	// a publish that already succeeded.
	for _, chunk := range chunkBatch(stale) {
		if err := c.submit(ctx, chunk); err != nil {
			slog.Warn("stale DNS records left in place", "domain", domain, "records", len(chunk.Deletes), "err", err)
			return changed, nil
		}
		changed += chunk.len()
	}
	return changed, nil
}

func chunkBatch(batch cfBatch) []cfBatch {
	var out []cfBatch
	for _, group := range []struct {
		put  bool
		recs []cfRecord
	}{{true, batch.Puts}, {false, batch.Posts}} {
		for start := 0; start < len(group.recs); start += cloudflareBatchMax {
			end := min(start+cloudflareBatchMax, len(group.recs))
			if group.put {
				out = append(out, cfBatch{Puts: group.recs[start:end]})
			} else {
				out = append(out, cfBatch{Posts: group.recs[start:end]})
			}
		}
	}
	for start := 0; start < len(batch.Deletes); start += cloudflareBatchMax {
		end := min(start+cloudflareBatchMax, len(batch.Deletes))
		out = append(out, cfBatch{Deletes: batch.Deletes[start:end]})
	}
	return out
}
