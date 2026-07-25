package snapshot

import (
	"context"
	"fmt"

	"github.com/MysticRyuujin/enrscout/internal/nodeset"
	"github.com/MysticRyuujin/enrscout/internal/store"
)

// LoadNetworkRows performs the shared integrity, decode, and row validation
// pipeline used by non-DuckDB snapshot consumers.
func LoadNetworkRows(ctx context.Context, st store.Store, layout Layout, manifest *Manifest, network string) ([]nodeset.Row, error) {
	ns, ok := manifest.Networks[network]
	if !ok {
		return nil, fmt.Errorf("network %q not in manifest", network)
	}
	data, err := st.Get(ctx, ns.GenerationKey)
	if err != nil {
		return nil, fmt.Errorf("get snapshot: %w", err)
	}
	if err := layout.VerifyGeneration(network, ns, data); err != nil {
		return nil, fmt.Errorf("snapshot integrity: %w", err)
	}
	rows, err := nodeset.RowsFromParquet(data)
	if err != nil {
		return nil, fmt.Errorf("decode snapshot: %w", err)
	}
	if err := ValidateRows(network, ns.NodeCount, rows); err != nil {
		return nil, err
	}
	return rows, nil
}

func ValidateRows(network string, expected int, rows []nodeset.Row) error {
	if len(rows) != expected {
		return fmt.Errorf("snapshot %s row count mismatch: got %d want %d", network, len(rows), expected)
	}
	seen := make(map[string]struct{}, len(rows))
	for _, row := range rows {
		if row.Network != network {
			return fmt.Errorf("snapshot %s contains a row for network %q", network, row.Network)
		}
		if row.ID == "" {
			return fmt.Errorf("snapshot %s contains an empty node id", network)
		}
		if _, ok := seen[row.ID]; ok {
			return fmt.Errorf("snapshot %s contains duplicate node id %q", network, row.ID)
		}
		seen[row.ID] = struct{}{}
	}
	return nil
}
