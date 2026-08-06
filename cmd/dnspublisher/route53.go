package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/route53"
	"github.com/aws/aws-sdk-go-v2/service/route53/types"
)

const (
	// A change batch is capped at 1000 changes and 32000 bytes of RDATA, and an UPSERT counts twice.
	// https://docs.aws.amazon.com/Route53/latest/DeveloperGuide/DNSLimitations.html#limits-api-requests-changeresourcerecordsets
	route53ChangeCountLimit = 1000
	route53ChangeSizeLimit  = 32000
	// A TXT character-string holds at most 255 bytes. devp2p splits at 253, and matching it keeps a
	// zone it already published free of rewrites when this binary takes the zone over.
	route53ChunkBytes   = 253
	route53ListMaxItems = 1000
	route53Timeout      = 30 * time.Second
	route53SyncPoll     = 5 * time.Second
	route53SyncTimeout  = 5 * time.Minute

	maxRoute53CredentialsBytes = 1 << 12
)

// route53API is the slice of Route53 this publisher calls, so tests drive Sync without standing up an
// XML endpoint for the SDK to parse.
type route53API interface {
	ListResourceRecordSets(context.Context, *route53.ListResourceRecordSetsInput, ...func(*route53.Options)) (*route53.ListResourceRecordSetsOutput, error)
	ChangeResourceRecordSets(context.Context, *route53.ChangeResourceRecordSetsInput, ...func(*route53.Options)) (*route53.ChangeResourceRecordSetsOutput, error)
	GetChange(context.Context, *route53.GetChangeInput, ...func(*route53.Options)) (*route53.GetChangeOutput, error)
}

type route53DNS struct {
	api         route53API
	zoneID      string
	poll        time.Duration
	syncTimeout time.Duration
}

type route53Creds struct {
	keyID  string
	secret string
}

func (c route53Creds) Retrieve(context.Context) (aws.Credentials, error) {
	return aws.Credentials{AccessKeyID: c.keyID, SecretAccessKey: c.secret, Source: "enrscout"}, nil
}

// newRoute53DNS builds a client from static credentials rather than the SDK's default chain: the
// chain would pull in SSO, STS, and instance-metadata lookups this publisher has no use for.
func newRoute53DNS(zoneID, region, keyID, secret string) *route53DNS {
	return &route53DNS{
		api: route53.New(route53.Options{
			Region:      region,
			Credentials: aws.NewCredentialsCache(route53Creds{keyID: keyID, secret: secret}),
			HTTPClient:  &http.Client{Timeout: route53Timeout},
		}),
		zoneID:      zoneID,
		poll:        route53SyncPoll,
		syncTimeout: route53SyncTimeout,
	}
}

// r53RecordSet keeps the stored values verbatim: Route53 rejects a DELETE unless it repeats the
// record's current values and TTL exactly.
type r53RecordSet struct {
	values []string
	ttl    int64
}

func (c *route53DNS) existing(ctx context.Context, domain string) (map[string]r53RecordSet, error) {
	out := map[string]r53RecordSet{}
	root := dnsKey(domain)
	suffix := "." + root
	req := &route53.ListResourceRecordSetsInput{
		HostedZoneId: aws.String(c.zoneID),
		MaxItems:     aws.Int32(route53ListMaxItems),
	}
	for {
		resp, err := c.api.ListResourceRecordSets(ctx, req)
		if err != nil {
			return nil, err
		}
		for _, set := range resp.ResourceRecordSets {
			if set.Type != types.RRTypeTxt || set.Name == nil || set.TTL == nil {
				continue
			}
			key := dnsKey(*set.Name)
			if key != root && !strings.HasSuffix(key, suffix) {
				continue
			}
			rec := r53RecordSet{ttl: *set.TTL}
			for _, rr := range set.ResourceRecords {
				if rr.Value != nil {
					rec.values = append(rec.values, *rr.Value)
				}
			}
			out[key] = rec
		}
		if !resp.IsTruncated {
			return out, nil
		}
		req.StartRecordName, req.StartRecordType = resp.NextRecordName, resp.NextRecordType
		req.StartRecordIdentifier = resp.NextRecordIdentifier
	}
}

// Sync reconciles the zone to want. Entries are written and confirmed in sync before the root so a
// resolver never follows a new root into a subtree that does not exist yet, and records are removed
// only once they are absent from both want and retain, so a client still holding the previous root
// can finish its walk.
func (c *route53DNS) Sync(ctx context.Context, domain string, want, retain map[string]string) (int, error) {
	have, err := c.existing(ctx, domain)
	if err != nil {
		return 0, fmt.Errorf("list records: %w", err)
	}
	wanted := make(map[string]string, len(want))
	for name, content := range want {
		wanted[dnsKey(name)] = content
	}
	var entries, root, stale []types.Change
	for name, content := range wanted {
		ttl := int64(ttlFor(name, domain))
		current, exists := have[name]
		if exists && normalizeTXT(strings.Join(current.values, "")) == content && current.ttl == ttl {
			continue
		}
		action := types.ChangeActionCreate
		if exists {
			action = types.ChangeActionUpsert
		}
		change := txtChange(action, name, ttl, splitTXT(content))
		if name == dnsKey(domain) {
			root = append(root, change)
			continue
		}
		entries = append(entries, change)
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
			stale = append(stale, txtChange(types.ChangeActionDelete, name, current.ttl, current.values...))
		}
	}
	sortChanges(entries)
	sortChanges(stale)

	ids, changed, err := c.submit(ctx, entries, "enrscout entries for "+domain)
	if err != nil {
		return changed, fmt.Errorf("write entries: %w", err)
	}
	// Ordering the API writes does not order propagation to Route53's authoritative servers, so the
	// entries must be resolvable before the root points at them. Failing here leaves entries the
	// still-current root does not reference, which is the safe direction to fail in.
	if err := c.waitForSync(ctx, ids); err != nil {
		return changed, fmt.Errorf("await entry propagation: %w", err)
	}
	_, n, err := c.submit(ctx, root, "enrscout root for "+domain)
	changed += n
	if err != nil {
		return changed, fmt.Errorf("write root: %w", err)
	}
	// A failed prune leaves records the current root simply does not reference, so it must not fail
	// a publish that already succeeded.
	_, n, err = c.submit(ctx, stale, "enrscout prune for "+domain)
	changed += n
	if err != nil {
		slog.Warn("stale DNS records left in place", "domain", domain, "records", len(stale), "err", err)
		return changed, nil
	}
	return changed, nil
}

func (c *route53DNS) submit(ctx context.Context, changes []types.Change, comment string) ([]string, int, error) {
	var ids []string
	accepted := 0
	batches := splitChanges(changes)
	for i, batch := range batches {
		resp, err := c.api.ChangeResourceRecordSets(ctx, &route53.ChangeResourceRecordSetsInput{
			HostedZoneId: aws.String(c.zoneID),
			ChangeBatch: &types.ChangeBatch{
				Changes: batch,
				Comment: aws.String(fmt.Sprintf("%s (%d/%d)", comment, i+1, len(batches))),
			},
		})
		if err != nil {
			return ids, accepted, err
		}
		accepted += len(batch)
		if resp.ChangeInfo != nil && resp.ChangeInfo.Id != nil {
			ids = append(ids, *resp.ChangeInfo.Id)
		}
	}
	return ids, accepted, nil
}

func (c *route53DNS) waitForSync(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	deadline := time.Now().Add(c.syncTimeout)
	for _, id := range ids {
		for {
			resp, err := c.api.GetChange(ctx, &route53.GetChangeInput{Id: aws.String(id)})
			if err != nil {
				return err
			}
			if resp.ChangeInfo != nil && resp.ChangeInfo.Status == types.ChangeStatusInsync {
				break
			}
			if !time.Now().Before(deadline) {
				return fmt.Errorf("change %s still pending after %s", id, c.syncTimeout)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(c.poll):
			}
		}
	}
	return nil
}

// splitChanges packs changes into batches inside Route53's RDATA and change-count limits. A batch is
// applied atomically, so one that exceeds either limit is rejected whole.
func splitChanges(changes []types.Change) [][]types.Change {
	var (
		batches [][]types.Change
		size    int
		count   int
	)
	for _, change := range changes {
		n := changeCount(change)
		bytes := changeSize(change) * n
		if len(batches) == 0 || size+bytes > route53ChangeSizeLimit || count+n > route53ChangeCountLimit {
			batches = append(batches, nil)
			size, count = 0, 0
		}
		batches[len(batches)-1] = append(batches[len(batches)-1], change)
		size += bytes
		count += n
	}
	return batches
}

func changeSize(change types.Change) int {
	size := 0
	for _, rr := range change.ResourceRecordSet.ResourceRecords {
		if rr.Value != nil {
			size += len(*rr.Value)
		}
	}
	return size
}

// An UPSERT is counted as both a delete and a create against the per-batch change limit.
func changeCount(change types.Change) int {
	if change.Action == types.ChangeActionUpsert {
		return 2
	}
	return 1
}

func sortChanges(changes []types.Change) {
	sort.Slice(changes, func(i, j int) bool {
		return *changes[i].ResourceRecordSet.Name < *changes[j].ResourceRecordSet.Name
	})
}

func txtChange(action types.ChangeAction, name string, ttl int64, values ...string) types.Change {
	records := make([]types.ResourceRecord, 0, len(values))
	for _, value := range values {
		records = append(records, types.ResourceRecord{Value: aws.String(value)})
	}
	return types.Change{
		Action: action,
		ResourceRecordSet: &types.ResourceRecordSet{
			Name:            aws.String(name),
			Type:            types.RRTypeTxt,
			TTL:             aws.Int64(ttl),
			ResourceRecords: records,
		},
	}
}

// splitTXT renders content as adjacent quoted character-strings, byte-for-byte as devp2p writes
// them, so a tree it already published compares equal instead of being rewritten.
func splitTXT(content string) string {
	var b strings.Builder
	for len(content) > 0 {
		n := min(len(content), route53ChunkBytes)
		b.WriteString(strconv.Quote(content[:n]))
		content = content[n:]
	}
	return b.String()
}

// parseRoute53Credentials accepts the shared AWS credentials file with a single profile, or plain
// KEY=VALUE lines. An unrecognized key is an error rather than a skipped line, so credentials this
// publisher cannot use - a session token, say - fail loudly instead of yielding a signing error.
func parseRoute53Credentials(data []byte) (keyID, secret string, err error) {
	profiles := 0
	for i, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			profiles++
			if profiles > 1 {
				return "", "", errors.New("route53 credentials hold more than one profile")
			}
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return "", "", fmt.Errorf("route53 credentials line %d is not key=value", i+1)
		}
		key = strings.TrimSpace(key)
		switch strings.ToLower(key) {
		case "aws_access_key_id":
			keyID = strings.TrimSpace(value)
		case "aws_secret_access_key":
			secret = strings.TrimSpace(value)
		default:
			return "", "", fmt.Errorf("route53 credentials line %d sets unsupported key %q", i+1, key)
		}
	}
	if keyID == "" || secret == "" {
		return "", "", errors.New("route53 credentials must set aws_access_key_id and aws_secret_access_key")
	}
	return keyID, secret, nil
}
