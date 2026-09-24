package main

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"

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
// shipped fork-ready release without an ENRScout release. It is reloaded when its content changes,
// compared by hash because a copy that preserves mtime and size would otherwise be missed; a file that
// fails to read or validate keeps the table already in use.
type releasesFile struct {
	path string
	sum  [sha256.Size]byte
}

func (f *releasesFile) load() error {
	data, err := readBounded(f.path)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	if sum == f.sum {
		return nil
	}
	table, err := decodeReleases(data)
	if err != nil {
		return fmt.Errorf("decode %s: %w", f.path, err)
	}
	if err := netconf.SetClientReleases(table); err != nil {
		return fmt.Errorf("validate %s: %w", f.path, err)
	}
	f.sum = sum
	slog.Info("client release table loaded", "file", f.path, "updated", table.Updated, "entries", len(table.Releases))
	return nil
}

// readBounded re-applies the size limit while reading, since the file can grow after the Stat.
func readBounded(path string) ([]byte, error) {
	fh, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer fh.Close()
	data, err := io.ReadAll(io.LimitReader(fh, maxReleasesFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxReleasesFileBytes {
		return nil, fmt.Errorf("%s is over the %d-byte limit", path, maxReleasesFileBytes)
	}
	return data, nil
}
