package devnetconfig

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/MysticRyuujin/enrscout/internal/netconf"
)

// An active Fulu prepends an implicit Electra entry ahead of the configured BPO entries, so the
// blob schedule is only correct when both halves are asserted together.
func TestLoadAssemblesCompleteBundle(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("genesis.json", `{
		"config": {"chainId": 3151908, "homesteadBlock": 0, "eip150Block": 0,
			"eip155Block": 0, "eip158Block": 0, "byzantiumBlock": 0, "constantinopleBlock": 0,
			"petersburgBlock": 0, "istanbulBlock": 0, "berlinBlock": 0, "londonBlock": 0,
			"mergeNetsplitBlock": 0, "terminalTotalDifficulty": 0,
			"shanghaiTime": 0, "cancunTime": 0, "pragueTime": 0, "osakaTime": 0},
		"difficulty": "0x1", "gasLimit": "0x1c9c380", "timestamp": "0x0", "alloc": {}
	}`)
	write("network_id.txt", "3151909\n")
	write("config.yaml", `PRESET_BASE: mainnet
GENESIS_TIME: 1700000000
SECONDS_PER_SLOT: 12
GENESIS_FORK_VERSION: 0x10000038
ELECTRA_FORK_VERSION: 0x60000038
ELECTRA_FORK_EPOCH: 5
FULU_FORK_VERSION: 0x70000038
FULU_FORK_EPOCH: 10
BLOB_SCHEDULE:
  - EPOCH: 20
    MAX_BLOBS_PER_BLOCK: 15
`)
	write("genesis_validators_root.txt", "0x7033d675f49ab2e76ffba52871d1ff7b73914b3a5ac8e1c0986c19549d67c0d7\n")
	write("bootnodes.txt", "enode://a979fb575495b8d6db44f750317d0f4622bf4c2aa3365d6af7c284339968eef29b69ad0dce72a4d8db5ebb4968de0e3bec910127f134779fbcb0cb6d3331163c@52.16.188.185:30303\n")

	cfg, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.NetworkID != 3151909 {
		t.Errorf("NetworkID = %d, want 3151909 (network_id.txt, not the chain id)", cfg.NetworkID)
	}
	if cfg.GenesisValidatorsRoot != "0x7033d675f49ab2e76ffba52871d1ff7b73914b3a5ac8e1c0986c19549d67c0d7" {
		t.Errorf("GenesisValidatorsRoot = %q", cfg.GenesisValidatorsRoot)
	}
	if cfg.GenesisTime != 1700000000 || cfg.SecondsPerSlot != 12 || cfg.SlotsPerEpoch != 32 {
		t.Errorf("timing = %d/%d/%d, want 1700000000/12/32", cfg.GenesisTime, cfg.SecondsPerSlot, cfg.SlotsPerEpoch)
	}
	if cfg.FuluForkEpoch != 10 {
		t.Errorf("FuluForkEpoch = %d, want 10", cfg.FuluForkEpoch)
	}
	if len(cfg.CLForks) != 3 {
		t.Errorf("CLForks = %v, want genesis/electra/fulu", cfg.CLForks)
	}
	if len(cfg.BootnodeRecords) != 1 {
		t.Errorf("BootnodeRecords = %v, want one record", cfg.BootnodeRecords)
	}
	if len(cfg.ELGenesisJSON) == 0 {
		t.Error("ELGenesisJSON is empty")
	}
	want := []netconf.BlobParams{{Epoch: 5, MaxBlobs: 9}, {Epoch: 20, MaxBlobs: 15}}
	if !slices.Equal(cfg.BlobSchedule, want) {
		t.Errorf("BlobSchedule = %v, want implicit Electra fallback then the configured BPO %v", cfg.BlobSchedule, want)
	}
}

func TestParseCLConfigAcceptsKurtosisMainnetPreset(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	body := `PRESET_BASE: 'mainnet'
GENESIS_TIME: 1700000000
GENESIS_FORK_VERSION: 0x10000038
ELECTRA_FORK_VERSION: "0x60000038"
ELECTRA_FORK_EPOCH: 0
FULU_FORK_VERSION: 0x70000038
FULU_FORK_EPOCH: 0
SECONDS_PER_SLOT: 12
BLOB_SCHEDULE:
  - EPOCH: 0
    MAX_BLOBS_PER_BLOCK: 15
  - EPOCH: 10
    MAX_BLOBS_PER_BLOCK: 21
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := parseCLConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.slotsPerEpoch != 32 || cfg.genesisTime != 1700000000 || len(cfg.blobSchedule) != 2 {
		t.Fatalf("unexpected config: slots=%d genesis=%d blobs=%v", cfg.slotsPerEpoch, cfg.genesisTime, cfg.blobSchedule)
	}
}

func TestParseCLConfigDerivesSecondsPerSlotFromSlotDurationMS(t *testing.T) {
	tests := map[string]struct {
		body    string
		want    uint64
		wantErr bool
	}{
		"slot duration only":       {body: "SLOT_DURATION_MS: 12000\n", want: 12},
		"explicit seconds wins":    {body: "SLOT_DURATION_MS: 6000\nSECONDS_PER_SLOT: 12\n", want: 12},
		"sub-second slot duration": {body: "SLOT_DURATION_MS: 6500\n", wantErr: true},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			body := "PRESET_BASE: 'mainnet'\nGENESIS_TIME: 1700000000\nGENESIS_FORK_VERSION: 0x10000038\n" + tc.body
			if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			cfg, err := parseCLConfig(path)
			if tc.wantErr {
				if err == nil {
					t.Fatal("sub-second SLOT_DURATION_MS was accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.secondsPerSlot != tc.want {
				t.Fatalf("secondsPerSlot = %d, want %d", cfg.secondsPerSlot, tc.want)
			}
		})
	}
}

func TestParseCLConfigRejectsMalformedRecognizedValues(t *testing.T) {
	tests := map[string]string{
		"fork epoch":            "FULU_FORK_VERSION: 0x70000038\nFULU_FORK_EPOCH: soon\n",
		"blob value":            "BLOB_SCHEDULE:\n  - EPOCH: 0\n    MAX_BLOBS_PER_BLOCK: many\n",
		"incomplete blob entry": "BLOB_SCHEDULE:\n  - EPOCH: 0\n",
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := parseCLConfig(path); err == nil {
				t.Fatal("malformed recognized config value was accepted")
			}
		})
	}
}

func TestParseCLConfigFuluDisabled(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	body := "PRESET_BASE: mainnet\nGENESIS_TIME: 1700000000\nSECONDS_PER_SLOT: 12\nGENESIS_FORK_VERSION: 0x10000038\nFULU_FORK_VERSION: 0x70000038\nFULU_FORK_EPOCH: 18446744073709551615\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := parseCLConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.fuluActive {
		t.Fatal("fulu should be inactive when FULU_FORK_EPOCH is far-future")
	}
}

func TestLoadRequiresNetworkIDFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "genesis.json"), []byte(`{"config":{"chainId":1}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil {
		t.Fatal("missing network_id.txt was accepted")
	}
}
