package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/MysticRyuujin/enrscout/internal/netconf"
)

// decodeReleases reads YAML, so the operator file can carry comments; JSON is valid YAML and loads too.
// Unknown keys are rejected, so a misspelt field fails loudly instead of being dropped.
func decodeReleases(data []byte) (netconf.ClientReleaseTable, error) {
	var table netconf.ClientReleaseTable
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&table); err != nil {
		return table, err
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return table, errors.New("the file must hold exactly one YAML document")
	}
	return table, nil
}

// maxReleasesFileBytes bounds an operator file; the built-in table is a few kilobytes.
const maxReleasesFileBytes = 1 << 20

// releasesFile overrides the built-in client release table, so an operator can publish a newly
// shipped fork-ready release without an ENRScout release. It is reloaded when it changes; a file
// that fails to read or validate keeps the table already in use.
type releasesFile struct {
	path    string
	modTime time.Time
	size    int64
}

func (f *releasesFile) load() error {
	info, err := os.Stat(f.path)
	if err != nil {
		return err
	}
	if info.ModTime().Equal(f.modTime) && info.Size() == f.size {
		return nil
	}
	if info.Size() > maxReleasesFileBytes {
		return fmt.Errorf("%s is %d bytes, over the %d-byte limit", f.path, info.Size(), maxReleasesFileBytes)
	}
	data, err := os.ReadFile(f.path)
	if err != nil {
		return err
	}
	table, err := decodeReleases(data)
	if err != nil {
		return fmt.Errorf("decode %s: %w", f.path, err)
	}
	if err := netconf.SetClientReleases(table); err != nil {
		return fmt.Errorf("validate %s: %w", f.path, err)
	}
	f.modTime, f.size = info.ModTime(), info.Size()
	slog.Info("client release table loaded", "file", f.path, "updated", table.Updated, "entries", len(table.Releases))
	return nil
}
