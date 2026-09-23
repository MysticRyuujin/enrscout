package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/MysticRyuujin/enrscout/internal/snapshot"
	"github.com/MysticRyuujin/enrscout/internal/store"
)

type output struct {
	SchemaVersion int               `json:"schema_version"`
	URL           string            `json:"url"`
	Domain        string            `json:"domain"`
	Network       string            `json:"network"`
	Capability    string            `json:"capability"`
	Nodes         int               `json:"nodes"`
	Seq           uint64            `json:"seq"`
	Records       map[string]string `json:"records"`
}

const outputSchemaVersion = 1

const publishedSuffix = ".published"

// baselineFor reads the last built artifact for domain. A zero-node artifact predates the
// empty-tree guard, so it yields no collapse baseline rather than wedging the domain forever, but
// its sequence still has to be exceeded.
func baselineFor(outDir, domain, network, capability string) (nodes int, seq uint64, err error) {
	prev, err := ownArtifact(outDir, domain, domain, network, capability)
	if err != nil || prev == nil {
		return 0, 0, err
	}
	return prev.Nodes, prev.Seq, nil
}

// publishedArtifact reads what a domain last got into DNS, which is nil until a push succeeds.
func publishedArtifact(outDir, domain, network, capability string) (*output, error) {
	return ownArtifact(outDir, domain+publishedSuffix, domain, network, capability)
}

func ownArtifact(outDir, name, domain, network, capability string) (*output, error) {
	if outDir == "" {
		return nil, nil
	}
	prev, err := readPrevious(outDir, name)
	if err != nil || prev == nil {
		return nil, err
	}
	if err := validateBaseline(prev, domain, network, capability); err != nil {
		return nil, err
	}
	return prev, nil
}

// Sequence floor tracks the last build, or a reused sequence leaves resolvers on a cached root for
// changed content. Collapse baseline tracks the last successful publish, or a build a failed push
// never delivered becomes the number the next cycle measures its drop against.
// The published artifact's records are returned too, so the push reuses them as its retain set.
func baselinesFor(cfg multiConfig, domain, network, capability string) (nodes int, seq uint64, retain map[string]string, err error) {
	nodes, seq, err = baselineFor(cfg.outDir, domain, network, capability)
	if err != nil || cfg.publisher == nil {
		return nodes, seq, nil, err
	}
	published, err := publishedArtifact(cfg.outDir, domain, network, capability)
	if err != nil {
		return 0, 0, nil, err
	}
	if published == nil {
		return 0, seq, nil, nil
	}
	return published.Nodes, seq, published.Records, nil
}

func emitArtifact(out output, outDir, name string) error {
	buf, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	if outDir == "" {
		_, err := os.Stdout.Write(append(buf, '\n'))
		return err
	}
	st, err := store.NewFS(outDir)
	if err != nil {
		return err
	}
	return st.Put(context.Background(), name+".json", buf, "application/json")
}

// readPrevious returns nil, nil when the artifact does not exist yet.
func readPrevious(outDir, name string) (*output, error) {
	st, err := store.NewFS(outDir)
	if err != nil {
		return nil, err
	}
	raw, err := st.Get(context.Background(), name+".json")
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var prev output
	if err := snapshot.UnmarshalStrict(raw, &prev); err != nil {
		return nil, err
	}
	return &prev, nil
}

func outputSequence(prev *output) (uint64, error) {
	if prev.SchemaVersion != outputSchemaVersion {
		return 0, fmt.Errorf("unsupported DNS artifact schema version %d", prev.SchemaVersion)
	}
	if prev.Seq == 0 {
		return 0, errors.New("previous artifact has no sequence")
	}
	return prev.Seq, nil
}

func validateBaseline(prev *output, domain, network, capability string) error {
	if _, err := outputSequence(prev); err != nil {
		return err
	}
	if prev.Domain != domain {
		return fmt.Errorf("artifact domain %q does not match %q", prev.Domain, domain)
	}
	if prev.Network != network {
		return fmt.Errorf("artifact network %q does not match %q", prev.Network, network)
	}
	if prev.Capability != capability {
		return fmt.Errorf("artifact capability %q does not match %q", prev.Capability, capability)
	}
	// Sequences are unix seconds and node counts small; implausible values from a corrupted
	// artifact would wrap treeSequence to 0 or overflow the collapse-guard arithmetic.
	const maxBaselineSeq = uint64(1) << 40
	const maxBaselineNodes = 1 << 24
	if prev.Seq > maxBaselineSeq {
		return fmt.Errorf("artifact sequence %d is implausible", prev.Seq)
	}
	if prev.Nodes < 0 || prev.Nodes > maxBaselineNodes {
		return fmt.Errorf("artifact reports %d nodes", prev.Nodes)
	}
	if len(prev.Records) == 0 {
		return errors.New("artifact has no TXT records")
	}
	return nil
}
