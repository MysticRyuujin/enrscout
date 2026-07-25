package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"golang.org/x/sys/unix"
)

var (
	ErrNotFound    = errors.New("object not found")
	ErrCASConflict = errors.New("conditional object write conflict")
)

const MaxObjectBytes = 128 << 20

var errBadKey = errors.New("invalid object key")

func safeRelPath(key string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(key))
	if clean == "." || filepath.IsAbs(clean) || clean == ".." ||
		strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: %q", errBadKey, key)
	}
	return clean, nil
}

type Store interface {
	Put(ctx context.Context, key string, data []byte, contentType string) error
	Get(ctx context.Context, key string) ([]byte, error)
	List(ctx context.Context, prefix string) ([]string, error)
	Delete(ctx context.Context, key string) error
	Backend() string
}

// ConditionalStore exposes opaque object versions for compare-and-swap. PutIfVersion is atomic in
// native mode; verified mode has the backend limitation documented on its implementation.
type ConditionalStore interface {
	Store
	GetVersion(ctx context.Context, key string) ([]byte, string, error)
	// PutIfVersion replaces the object when expectedVersion matches (or the object is
	// absent when expectedVersion is empty). On success it returns the new opaque version
	// so callers can delete or chain CAS without a follow-up GetVersion.
	PutIfVersion(ctx context.Context, key string, expectedVersion string, data []byte, contentType string) (string, error)
}

type fsStore struct {
	base string
}

// NewFS creates a store rooted in a trusted local directory. Symlink checks are
// defense in depth, not isolation from another process that can mutate the root.
func NewFS(base string) (Store, error) {
	if err := os.MkdirAll(base, 0o755); err != nil {
		return nil, err
	}
	base, err := filepath.Abs(base)
	if err != nil {
		return nil, err
	}
	base, err = filepath.EvalSymlinks(base)
	if err != nil {
		return nil, err
	}
	return &fsStore{base: base}, nil
}

func (s *fsStore) Put(_ context.Context, key string, data []byte, _ string) error {
	if len(data) > MaxObjectBytes {
		return fmt.Errorf("object %q exceeds %d bytes", key, MaxObjectBytes)
	}
	rel, err := safeRelPath(key)
	if err != nil {
		return err
	}
	dst := filepath.Join(s.base, rel)
	if err := rejectSymlinkComponents(s.base, filepath.Dir(dst)); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".enrscout-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return err
	}
	return syncDir(filepath.Dir(dst))
}

func syncDir(path string) error {
	d, err := os.Open(path)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func (s *fsStore) Get(_ context.Context, key string) ([]byte, error) {
	rel, err := safeRelPath(key)
	if err != nil {
		return nil, err
	}
	dst := filepath.Join(s.base, rel)
	if err := rejectSymlinkComponents(s.base, dst); err != nil {
		return nil, err
	}
	fi, err := os.Stat(dst)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if fi.Size() > MaxObjectBytes {
		return nil, fmt.Errorf("object %q too large: %d bytes", key, fi.Size())
	}
	data, err := os.ReadFile(dst)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	return data, err
}

func objectVersion(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (s *fsStore) GetVersion(ctx context.Context, key string) ([]byte, string, error) {
	data, err := s.Get(ctx, key)
	if err != nil {
		return nil, "", err
	}
	return data, objectVersion(data), nil
}

func (s *fsStore) PutIfVersion(ctx context.Context, key, expectedVersion string, data []byte, contentType string) (string, error) {
	rel, err := safeRelPath(key)
	if err != nil {
		return "", err
	}
	dst := filepath.Join(s.base, rel)
	if err := rejectSymlinkComponents(s.base, filepath.Dir(dst)); err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", err
	}
	lock := dst + ".lock"
	lockFile, err := acquireFileLock(ctx, lock)
	if err != nil {
		return "", err
	}
	defer func() {
		_ = unix.Flock(int(lockFile.Fd()), unix.LOCK_UN)
		_ = lockFile.Close()
	}()

	current, getErr := s.Get(ctx, key)
	if errors.Is(getErr, ErrNotFound) {
		if expectedVersion != "" {
			return "", ErrCASConflict
		}
	} else if getErr != nil {
		return "", getErr
	} else if expectedVersion == "" || objectVersion(current) != expectedVersion {
		return "", ErrCASConflict
	}
	if err := s.Put(ctx, key, data, contentType); err != nil {
		return "", err
	}
	return objectVersion(data), nil
}

func acquireFileLock(ctx context.Context, path string) (*os.File, error) {
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("refusing symlink lock %q", path)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	for {
		f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
		if err != nil {
			return nil, err
		}
		if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err == nil {
			return f, nil
		} else if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			_ = f.Close()
			return nil, err
		}
		_ = f.Close()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// List walks only the subtree the prefix's directory part selects, so a caller listing one network's
// generations does not stat every other object in the store. Keys stay relative to the store root,
// and the prefix need not align to a directory boundary.
func (s *fsStore) List(_ context.Context, prefix string) ([]string, error) {
	root := s.base
	if dir := path.Dir(prefix); dir != "." && dir != "/" {
		rel, err := safeRelPath(dir)
		if err != nil {
			return nil, err
		}
		root = filepath.Join(s.base, rel)
	}
	if _, err := os.Stat(root); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	var keys []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		rel, err := filepath.Rel(s.base, path)
		if err != nil {
			return err
		}
		key := filepath.ToSlash(rel)
		if strings.HasSuffix(key, ".tmp") || strings.HasSuffix(key, ".lock") {
			return nil
		}
		if strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return keys, nil
}

func (s *fsStore) Delete(_ context.Context, key string) error {
	rel, err := safeRelPath(key)
	if err != nil {
		return err
	}
	dst := filepath.Join(s.base, rel)
	if err := rejectSymlinkComponents(s.base, dst); err != nil {
		return err
	}
	err = os.Remove(dst)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return syncDir(filepath.Dir(dst))
}

func rejectSymlinkComponents(base, path string) error {
	rel, err := filepath.Rel(base, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("%w: path escapes store", errBadKey)
	}
	cur := base
	for _, component := range strings.Split(rel, string(filepath.Separator)) {
		if component == "" || component == "." {
			continue
		}
		cur = filepath.Join(cur, component)
		info, statErr := os.Lstat(cur)
		if errors.Is(statErr, os.ErrNotExist) {
			return nil
		}
		if statErr != nil {
			return statErr
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: symlink component %q", errBadKey, component)
		}
	}
	return nil
}

func (s *fsStore) Backend() string { return "fs:" + s.base }

type S3Config struct {
	Endpoint     string
	Region       string
	Bucket       string
	AccessKey    string
	SecretKey    string
	UseSSL       bool
	CreateBucket bool
	// ConditionalMode controls manifest compare-and-swap behavior. "native"
	// requires PutObject If-Match support. "verified" is for partial-S3
	// services: it prechecks the version, writes, and verifies the result, but
	// it is not atomic across hosts and therefore requires external election.
	ConditionalMode string
}

func Open(ctx context.Context, cfg S3Config, dir string) (Store, error) {
	if cfg.Endpoint == "" {
		return NewFS(dir)
	}
	return NewS3(ctx, cfg)
}

type s3Store struct {
	client          *minio.Client
	bucket          string
	desc            string
	conditionalMode string
}

func NewS3(ctx context.Context, cfg S3Config) (Store, error) {
	if strings.TrimSpace(cfg.Endpoint) == "" {
		return nil, errors.New("s3 endpoint is required")
	}
	if strings.TrimSpace(cfg.Bucket) == "" {
		return nil, errors.New("s3 bucket is required")
	}
	if (cfg.AccessKey == "") != (cfg.SecretKey == "") {
		return nil, errors.New("s3 access key and secret key must be provided together")
	}
	mode := strings.TrimSpace(cfg.ConditionalMode)
	if mode == "" {
		mode = "native"
	}
	if mode != "native" && mode != "verified" {
		return nil, fmt.Errorf("s3 conditional mode must be native or verified, got %q", cfg.ConditionalMode)
	}
	client, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure: cfg.UseSSL,
		Region: cfg.Region,
	})
	if err != nil {
		return nil, fmt.Errorf("s3 client: %w", err)
	}
	if err := ensureBucket(ctx, client, cfg); err != nil {
		return nil, err
	}
	return &s3Store{
		client: client, bucket: cfg.Bucket,
		desc: fmt.Sprintf("s3:%s/%s (conditional=%s)", cfg.Endpoint, cfg.Bucket, mode), conditionalMode: mode,
	}, nil
}

type bucketClient interface {
	BucketExists(context.Context, string) (bool, error)
	MakeBucket(context.Context, string, minio.MakeBucketOptions) error
}

func ensureBucket(ctx context.Context, client *minio.Client, cfg S3Config) error {
	return ensureBucketWithInterval(ctx, client, cfg, time.Second)
}

func ensureBucketWithInterval(ctx context.Context, client bucketClient, cfg S3Config, interval time.Duration) error {
	var last error
	for attempt := 0; attempt < 15; attempt++ {
		ok, err := client.BucketExists(ctx, cfg.Bucket)
		if err != nil {
			last = err
		} else if ok {
			return nil
		} else if !cfg.CreateBucket {
			last = fmt.Errorf("bucket %q does not exist", cfg.Bucket)
		} else if err := client.MakeBucket(ctx, cfg.Bucket, minio.MakeBucketOptions{Region: cfg.Region}); err == nil {
			return nil
		} else {
			last = err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
	return fmt.Errorf("bucket %q not ready: %w", cfg.Bucket, last)
}

func (s *s3Store) Put(ctx context.Context, key string, data []byte, contentType string) error {
	if _, err := safeRelPath(key); err != nil {
		return err
	}
	if len(data) > MaxObjectBytes {
		return fmt.Errorf("object %q exceeds %d bytes", key, MaxObjectBytes)
	}
	_, err := s.client.PutObject(ctx, s.bucket, key, bytes.NewReader(data), int64(len(data)),
		minio.PutObjectOptions{ContentType: contentType})
	return err
}

func (s *s3Store) Get(ctx context.Context, key string) ([]byte, error) {
	if _, err := safeRelPath(key); err != nil {
		return nil, err
	}
	obj, err := s.client.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, err
	}
	defer obj.Close()
	data, err := io.ReadAll(io.LimitReader(obj, MaxObjectBytes+1))
	if err != nil {
		resp := minio.ToErrorResponse(err)
		if resp.Code == "NoSuchKey" {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if len(data) > MaxObjectBytes {
		return nil, fmt.Errorf("object %q exceeds %d bytes", key, MaxObjectBytes)
	}
	return data, nil
}

func (s *s3Store) GetVersion(ctx context.Context, key string) ([]byte, string, error) {
	if _, err := safeRelPath(key); err != nil {
		return nil, "", err
	}
	info, err := s.client.StatObject(ctx, s.bucket, key, minio.StatObjectOptions{})
	if err != nil {
		if minio.ToErrorResponse(err).Code == "NoSuchKey" {
			return nil, "", ErrNotFound
		}
		return nil, "", err
	}
	opts := minio.GetObjectOptions{}
	if err := opts.SetMatchETag(info.ETag); err != nil {
		return nil, "", err
	}
	obj, err := s.client.GetObject(ctx, s.bucket, key, opts)
	if err != nil {
		return nil, "", err
	}
	defer obj.Close()
	data, err := io.ReadAll(io.LimitReader(obj, MaxObjectBytes+1))
	if err != nil {
		if minio.ToErrorResponse(err).Code == minio.PreconditionFailed {
			return nil, "", ErrCASConflict
		}
		return nil, "", err
	}
	if len(data) > MaxObjectBytes {
		return nil, "", fmt.Errorf("object %q exceeds %d bytes", key, MaxObjectBytes)
	}
	return data, info.ETag, nil
}

func (s *s3Store) PutIfVersion(ctx context.Context, key, expectedVersion string, data []byte, contentType string) (string, error) {
	if _, err := safeRelPath(key); err != nil {
		return "", err
	}
	if len(data) > MaxObjectBytes {
		return "", fmt.Errorf("object %q exceeds %d bytes", key, MaxObjectBytes)
	}
	if s.conditionalMode == "verified" {
		return s.putIfVersionVerified(ctx, key, expectedVersion, data, contentType)
	}
	opts := minio.PutObjectOptions{ContentType: contentType}
	if expectedVersion == "" {
		opts.SetMatchETagExcept("*")
	} else {
		opts.SetMatchETag(expectedVersion)
	}
	info, err := s.client.PutObject(ctx, s.bucket, key, bytes.NewReader(data), int64(len(data)), opts)
	if err != nil && minio.ToErrorResponse(err).Code == minio.PreconditionFailed {
		return "", ErrCASConflict
	}
	if err != nil {
		return "", err
	}
	return info.ETag, nil
}

func (s *s3Store) putIfVersionVerified(ctx context.Context, key, expectedVersion string, data []byte, contentType string) (string, error) {
	_, currentVersion, err := s.GetVersion(ctx, key)
	if errors.Is(err, ErrNotFound) {
		if expectedVersion != "" {
			return "", ErrCASConflict
		}
	} else if err != nil {
		return "", err
	} else if expectedVersion == "" || currentVersion != expectedVersion {
		return "", ErrCASConflict
	}
	if err := s.Put(ctx, key, data, contentType); err != nil {
		return "", err
	}
	written, version, err := s.GetVersion(ctx, key)
	if err != nil {
		return "", err
	}
	if !bytes.Equal(written, data) {
		return "", ErrCASConflict
	}
	return version, nil
}

func (s *s3Store) List(ctx context.Context, prefix string) ([]string, error) {
	if prefix != "" {
		if _, err := safeRelPath(strings.TrimSuffix(prefix, "/")); err != nil {
			return nil, err
		}
	}
	var keys []string
	for obj := range s.client.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{Prefix: prefix, Recursive: true}) {
		if obj.Err != nil {
			return nil, obj.Err
		}
		keys = append(keys, obj.Key)
	}
	return keys, nil
}

func (s *s3Store) Delete(ctx context.Context, key string) error {
	if _, err := safeRelPath(key); err != nil {
		return err
	}
	return s.client.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{})
}

func (s *s3Store) Backend() string { return s.desc }
