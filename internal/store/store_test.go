package store

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"golang.org/x/sys/unix"
)

type delayedBucketClient struct {
	calls      int
	readyAfter int
}

func (c *delayedBucketClient) BucketExists(context.Context, string) (bool, error) {
	c.calls++
	return c.calls >= c.readyAfter, nil
}

func (*delayedBucketClient) MakeBucket(context.Context, string, minio.MakeBucketOptions) error {
	return errors.New("unexpected bucket creation")
}

func TestFSPutGetRoundTrip(t *testing.T) {
	st, err := NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	want := []byte("parquet-bytes")
	if err := st.Put(ctx, "snapshots/mainnet/latest.parquet", want, "application/octet-stream"); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, err := st.Get(ctx, "snapshots/mainnet/latest.parquet")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("round-trip mismatch: got %q want %q", got, want)
	}
}

func TestFSGetMissingReturnsNotFound(t *testing.T) {
	st, err := NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Get(context.Background(), "snapshots/nope/latest.parquet"); !errors.Is(err, ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func TestFSRejectsPathTraversal(t *testing.T) {
	st, err := NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, k := range []string{"../escape", "../../etc/passwd", "/abs/path", "a/../../b"} {
		if err := st.Put(ctx, k, []byte("x"), ""); err == nil {
			t.Errorf("Put(%q) should be rejected", k)
		}
		if _, err := st.Get(ctx, k); err == nil {
			t.Errorf("Get(%q) should be rejected", k)
		}
		if err := st.Delete(ctx, k); err == nil {
			t.Errorf("Delete(%q) should be rejected", k)
		}
	}
	if err := st.Put(ctx, "snapshots/ok.parquet", []byte("ok"), ""); err != nil {
		t.Errorf("normal key should be accepted: %v", err)
	}
}

func TestFSRejectsSymlinkEscape(t *testing.T) {
	base := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(base, "linked")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	st, err := NewFS(base)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := st.Put(ctx, "linked/object", []byte("x"), ""); err == nil {
		t.Fatal("put through symlink was accepted")
	}
	if _, err := st.Get(ctx, "linked/object"); err == nil {
		t.Fatal("get through symlink was accepted")
	}
}

func TestFSListAndDelete(t *testing.T) {
	st, err := NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, k := range []string{
		"snapshots/mainnet/a.parquet", "snapshots/mainnet/b.parquet",
		"snapshots/hoodi/c.parquet", "other/d.txt",
	} {
		if err := st.Put(ctx, k, []byte("x"), ""); err != nil {
			t.Fatal(err)
		}
	}

	got, err := st.List(ctx, "snapshots/mainnet/")
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(got)
	want := []string{"snapshots/mainnet/a.parquet", "snapshots/mainnet/b.parquet"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("List = %v, want %v", got, want)
	}

	if err := st.Delete(ctx, "snapshots/mainnet/a.parquet"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	got, _ = st.List(ctx, "snapshots/mainnet/")
	if len(got) != 1 || got[0] != "snapshots/mainnet/b.parquet" {
		t.Errorf("after delete List = %v, want [b]", got)
	}
	if err := st.Delete(ctx, "snapshots/mainnet/missing.parquet"); err != nil {
		t.Errorf("delete of missing key should be nil, got %v", err)
	}
}

func TestFSListWalksOnlyThePrefixSubtree(t *testing.T) {
	st, err := NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, key := range []string{"snapshots/mainnet/a.parquet", "snapshots/mainnet/b.parquet", "snapshots/hoodi/c.parquet", "snapshots/aggregates/m/d.json"} {
		if err := st.Put(ctx, key, []byte("x"), "application/octet-stream"); err != nil {
			t.Fatal(err)
		}
	}
	got, err := st.List(ctx, "snapshots/mainnet/")
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(got)
	if want := []string{"snapshots/mainnet/a.parquet", "snapshots/mainnet/b.parquet"}; len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("List = %v, want %v", got, want)
	}
	// A prefix that does not align to a directory boundary must still filter within its parent.
	partial, err := st.List(ctx, "snapshots/mainnet/a")
	if err != nil || len(partial) != 1 || partial[0] != "snapshots/mainnet/a.parquet" {
		t.Fatalf("partial prefix List = %v, %v", partial, err)
	}
	missing, err := st.List(ctx, "snapshots/sepolia/")
	if err != nil || len(missing) != 0 {
		t.Fatalf("missing subtree List = %v, %v", missing, err)
	}
	all, err := st.List(ctx, "")
	if err != nil || len(all) != 4 {
		t.Fatalf("empty prefix List = %v, %v", all, err)
	}
}

func TestFSConditionalWrite(t *testing.T) {
	st, err := NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cs := st.(ConditionalStore)
	ctx := context.Background()
	if _, err := cs.PutIfVersion(ctx, "manifest.json", "", []byte("one"), "application/json"); err != nil {
		t.Fatal(err)
	}
	_, version, err := cs.GetVersion(ctx, "manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cs.PutIfVersion(ctx, "manifest.json", "stale", []byte("bad"), "application/json"); !errors.Is(err, ErrCASConflict) {
		t.Fatalf("stale version error = %v, want ErrCASConflict", err)
	}
	if got, err := cs.PutIfVersion(ctx, "manifest.json", version, []byte("two"), "application/json"); err != nil {
		t.Fatal(err)
	} else if got == "" || got == version {
		t.Fatalf("new version = %q, want a distinct post-write version", got)
	}
	got, err := st.Get(ctx, "manifest.json")
	if err != nil || string(got) != "two" {
		t.Fatalf("conditional result = %q, %v", got, err)
	}
}

func TestFSConditionalWriteRecoversExistingLockFile(t *testing.T) {
	base := t.TempDir()
	st, err := NewFS(base)
	if err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(base, "manifest.json.lock")
	if err := os.WriteFile(lock, []byte("left by crashed writer"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := st.(ConditionalStore).PutIfVersion(context.Background(), "manifest.json", "", []byte("one"), "application/json"); err != nil {
		t.Fatalf("conditional write did not recover an unlocked lock file: %v", err)
	}
}

func TestFSConditionalWriteWaitsForLiveLock(t *testing.T) {
	base := t.TempDir()
	st, err := NewFS(base)
	if err != nil {
		t.Fatal(err)
	}
	lock, err := os.OpenFile(filepath.Join(base, "manifest.json.lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	defer unix.Flock(int(lock.Fd()), unix.LOCK_UN)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err = st.(ConditionalStore).PutIfVersion(ctx, "manifest.json", "", []byte("one"), "application/json")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("live lock error = %v, want context deadline", err)
	}
}

func TestS3ConfigRejectsPartialCredentials(t *testing.T) {
	_, err := NewS3(context.Background(), S3Config{
		Endpoint: "localhost:9000", Bucket: "test", AccessKey: "only-access-key",
	})
	if err == nil {
		t.Fatal("partial S3 credentials were accepted")
	}
}

func TestS3ConfigRejectsUnknownConditionalMode(t *testing.T) {
	_, err := NewS3(context.Background(), S3Config{
		Endpoint: "localhost:9000", Bucket: "test", ConditionalMode: "hopeful",
	})
	if err == nil {
		t.Fatal("unknown S3 conditional mode was accepted")
	}
}

func TestEnsureBucketWaitsForExternalCreator(t *testing.T) {
	client := &delayedBucketClient{readyAfter: 3}
	err := ensureBucketWithInterval(context.Background(), client, S3Config{Bucket: "snapshots"}, time.Nanosecond)
	if err != nil {
		t.Fatal(err)
	}
	if client.calls != 3 {
		t.Fatalf("bucket checks = %d, want 3", client.calls)
	}
}
