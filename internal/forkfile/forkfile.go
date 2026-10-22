// Package forkfile reads the operator fork file: client release labels, notices, and cancelled
// forks. The API hot-reloads releases and notices; cancellations change the schedule every binary
// computes, so they apply only at startup.
package forkfile

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"reflect"
	"sync/atomic"

	"go.yaml.in/yaml/v3"

	"github.com/MysticRyuujin/enrscout/internal/netconf"
)

type File struct {
	netconf.ClientReleaseTable `yaml:",inline"`
	CancelledForks             []netconf.CancelledFork `yaml:"cancelled_forks,omitempty"`
	Notices                    []netconf.Notice        `yaml:"notices,omitempty"`
}

// Decode reads YAML, so the operator file can carry comments; JSON is valid YAML and loads too.
// Unknown keys are rejected, so a misspelt field fails loudly instead of being dropped.
func Decode(data []byte) (File, error) {
	var f File
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		return f, err
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return f, errors.New("the file must hold exactly one YAML document")
	}
	return f, nil
}

// maxBytes bounds an operator file; a fork's full table is a few kilobytes.
const maxBytes = 1 << 20

func read(path string) (File, [sha256.Size]byte, error) {
	fh, err := os.Open(path)
	if err != nil {
		return File{}, [sha256.Size]byte{}, err
	}
	defer fh.Close()
	data, err := io.ReadAll(io.LimitReader(fh, maxBytes+1))
	if err != nil {
		return File{}, [sha256.Size]byte{}, err
	}
	if len(data) > maxBytes {
		return File{}, [sha256.Size]byte{}, fmt.Errorf("%s is over the %d-byte limit", path, maxBytes)
	}
	f, err := Decode(data)
	if err != nil {
		return File{}, [sha256.Size]byte{}, fmt.Errorf("decode %s: %w", path, err)
	}
	return f, sha256.Sum256(data), nil
}

// ApplyCancellations reads only the cancellations, for the crawler and the DNS publisher. Call it at
// startup, after devnet registration and before any network is used.
func ApplyCancellations(path string) error {
	f, _, err := read(path)
	if err != nil {
		return err
	}
	return applyCancellations(path, f.CancelledForks)
}

func applyCancellations(path string, list []netconf.CancelledFork) error {
	absent, err := netconf.ApplyCancellations(list)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	for _, a := range absent {
		slog.Info("cancelled fork already absent from the compiled schedule; tracking only", "file", path, "fork", a)
	}
	for _, c := range list {
		slog.Warn("fork cancelled by the operator fork file", "file", path, "network", c.Network, "fork", c.Fork)
	}
	return nil
}

// Watcher keeps the API's releases and notices in step with the file. It compares content by hash,
// because a copy that preserves mtime and size would otherwise be missed; a file that fails to read or
// validate keeps what is already in use.
type Watcher struct {
	path            string
	sum             [sha256.Size]byte
	cancelled       []netconf.CancelledFork
	restartRequired atomic.Bool
}

func Open(path string) (*Watcher, error) {
	f, sum, err := read(path)
	if err != nil {
		return nil, err
	}
	if err := applyCancellations(path, f.CancelledForks); err != nil {
		return nil, err
	}
	w := &Watcher{path: path, cancelled: f.CancelledForks}
	if err := w.set(f, sum); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *Watcher) Reload() error {
	f, sum, err := read(w.path)
	if err != nil {
		return err
	}
	if sum == w.sum {
		return nil
	}
	changed := !reflect.DeepEqual(f.CancelledForks, w.cancelled)
	if changed {
		if err := netconf.ValidateCancellations(f.CancelledForks); err != nil {
			return fmt.Errorf("validate %s: a restart would fail: %w", w.path, err)
		}
	}
	if err := w.set(f, sum); err != nil {
		return err
	}
	w.restartRequired.Store(changed)
	if changed {
		slog.Warn("cancelled forks changed; restart crawler, dnspublisher and api to apply them", "file", w.path)
	}
	return nil
}

func (w *Watcher) RestartRequired() bool { return w.restartRequired.Load() }

func (w *Watcher) set(f File, sum [sha256.Size]byte) error {
	if err := netconf.SetForkFileData(f.ClientReleaseTable, f.Notices); err != nil {
		return fmt.Errorf("validate %s: %w", w.path, err)
	}
	w.sum = sum
	slog.Info("fork file loaded", "file", w.path, "updated", f.Updated, "releases", len(f.Releases),
		"notices", len(f.Notices), "cancelled_forks", len(f.CancelledForks))
	return nil
}
