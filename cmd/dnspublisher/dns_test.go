package main

import (
	"context"
	"testing"
	"time"
)

func TestValidateTTLs(t *testing.T) {
	cases := []struct {
		name       string
		entry      time.Duration
		root       time.Duration
		cloudflare bool
		want       recordTTLs
		wantErr    bool
	}{
		{name: "defaults", entry: entryTTL * time.Second, root: rootTTL * time.Second, want: defaultTTLs},
		{name: "devp2p values", entry: 4 * 7 * 24 * time.Hour, root: 30 * time.Minute, want: recordTTLs{entry: 2419200, root: 1800}},
		{name: "zero entry", entry: 0, root: rootTTL * time.Second, wantErr: true},
		{name: "sub-second root", entry: entryTTL * time.Second, root: 500 * time.Millisecond, wantErr: true},
		{name: "fractional seconds", entry: 1500 * time.Millisecond, root: rootTTL * time.Second, wantErr: true},
		{name: "past 31 bits", entry: (1<<31 + 1) * time.Second, root: rootTTL * time.Second, wantErr: true},
		{name: "cloudflare at the cap", entry: cloudflareMaxTTL, root: rootTTL * time.Second, cloudflare: true, want: recordTTLs{entry: 86400, root: rootTTL}},
		{name: "cloudflare past the cap", entry: cloudflareMaxTTL + time.Second, root: rootTTL * time.Second, cloudflare: true, wantErr: true},
		{name: "cloudflare below the floor", entry: entryTTL * time.Second, root: 30 * time.Second, cloudflare: true, wantErr: true},
		{name: "cloudflare automatic sentinel", entry: entryTTL * time.Second, root: time.Second, cloudflare: true, wantErr: true},
		{name: "route53 below the cloudflare floor", entry: entryTTL * time.Second, root: 30 * time.Second, want: recordTTLs{entry: entryTTL, root: 30}},
		{name: "cloudflare root past the cap", entry: entryTTL * time.Second, root: cloudflareMaxTTL + time.Second, cloudflare: true, wantErr: true},
		{name: "route53 past the cloudflare cap", entry: 4 * 7 * 24 * time.Hour, root: rootTTL * time.Second, want: recordTTLs{entry: 2419200, root: rootTTL}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := validateTTLs(tc.entry, tc.root, tc.cloudflare)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("got %+v, want an error", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestRoute53AppliesConfiguredTTLs(t *testing.T) {
	long := recordTTLs{entry: 2419200, root: 1800}
	want := r53Tree(1)
	z := newFakeRoute53()
	for name, content := range want {
		z.seedTXT(name, content, int64(defaultTTLs.forName(dnsKey(name), r53Domain)))
	}
	c := fakeRoute53DNS(t, z)
	c.ttls = long

	changed, err := c.Sync(context.Background(), r53Domain, want, nil)
	if err != nil {
		t.Fatal(err)
	}
	if changed != len(want) {
		t.Errorf("changed %d records, want %d: a TTL change must rewrite every record", changed, len(want))
	}
	for name := range want {
		if got := z.stored[dnsKey(name)].ttl; got != int64(long.forName(dnsKey(name), r53Domain)) {
			t.Errorf("%s stored with TTL %d, want %d", name, got, long.forName(dnsKey(name), r53Domain))
		}
	}

	changed, err = c.Sync(context.Background(), r53Domain, want, nil)
	if err != nil {
		t.Fatal(err)
	}
	if changed != 0 {
		t.Errorf("changed %d records on an identical re-sync", changed)
	}
}

func TestCloudflareAppliesConfiguredTTLs(t *testing.T) {
	long := recordTTLs{entry: 86400, root: 1800}
	existing := cfRecord{Type: "TXT", Name: "AAA." + cfDomain, Content: "enr:-abc", TTL: entryTTL}
	z := newFakeZone(existing)
	c := fakeCloudflare(t, z)
	c.ttls = long

	want := map[string]string{
		cfDomain:          "enrtree-root:v1 seq=1",
		"AAA." + cfDomain: "enr:-abc",
	}
	changed, err := c.Sync(context.Background(), cfDomain, want, nil)
	if err != nil {
		t.Fatal(err)
	}
	if changed != len(want) {
		t.Errorf("changed %d records, want %d: a TTL change must rewrite every record", changed, len(want))
	}
	for name := range want {
		key := dnsKey(name)
		if got := z.records[key].TTL; got != long.forName(key, cfDomain) {
			t.Errorf("%s stored with TTL %d, want %d", name, got, long.forName(key, cfDomain))
		}
	}

	changed, err = c.Sync(context.Background(), cfDomain, want, nil)
	if err != nil {
		t.Fatal(err)
	}
	if changed != 0 {
		t.Errorf("changed %d records on an identical re-sync", changed)
	}
}
