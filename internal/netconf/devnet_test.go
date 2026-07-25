package netconf

import (
	"slices"
	"testing"
)

const devnetGenesisJSON = `{
	"config": {"chainId": 3151908, "homesteadBlock": 0, "eip150Block": 0,
		"eip155Block": 0, "eip158Block": 0, "byzantiumBlock": 0, "constantinopleBlock": 0,
		"petersburgBlock": 0, "istanbulBlock": 0, "berlinBlock": 0, "londonBlock": 0,
		"mergeNetsplitBlock": 0, "terminalTotalDifficulty": 0,
		"shanghaiTime": 0, "cancunTime": 0, "pragueTime": 0, "osakaTime": 0},
	"difficulty": "0x1", "gasLimit": "0x1c9c380", "timestamp": "0x0", "alloc": {}
}`

func devnetConfig() DevnetConfig {
	return DevnetConfig{
		ELGenesisJSON:         []byte(devnetGenesisJSON),
		NetworkID:             3151909,
		GenesisValidatorsRoot: "0x7033d675f49ab2e76ffba52871d1ff7b73914b3a5ac8e1c0986c19549d67c0d7",
		GenesisTime:           1700000000,
		SecondsPerSlot:        12,
		SlotsPerEpoch:         32,
		CLForks: []CLForkConfig{
			{Epoch: 0, Version: "0x10000038"},
			{Epoch: 0, Version: "0x60000038"},
			{Epoch: 0, Version: "0x70000038"},
		},
		FuluForkEpoch: 0,
		BlobSchedule:  []BlobParams{{Epoch: 0, MaxBlobs: 9}, {Epoch: 0, MaxBlobs: 15}},
		BootnodeRecords: []string{
			"enode://a979fb575495b8d6db44f750317d0f4622bf4c2aa3365d6af7c284339968eef29b69ad0dce72a4d8db5ebb4968de0e3bec910127f134779fbcb0cb6d3331163c@52.16.188.185:30303",
		},
	}
}

// RegisterDevnet mutates process-global registries and is deliberately singular, so a test that
// registers must undo it or every later run in the same binary fails. Safe only while no test in
// this package calls t.Parallel().
func registerDevnetForTest(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		delete(networks, "devnet")
		delete(bootnodes, "devnet")
		delete(clBootnodes, "devnet")
		clNetworks = slices.DeleteFunc(clNetworks, func(c *clNetwork) bool { return c.name == "devnet" })
		names = slices.DeleteFunc(names, func(n string) bool { return n == "devnet" })
	})
	if err := RegisterDevnet(devnetConfig()); err != nil {
		t.Fatalf("RegisterDevnet: %v", err)
	}
}

func TestRegisterDevnetClassifiesBothLayers(t *testing.T) {
	registerDevnetForTest(t)

	nw, err := Get("devnet")
	if err != nil {
		t.Fatalf("devnet not registered: %v", err)
	}
	if got := Classify(nw.CurrentForkID()); got != "devnet" {
		t.Errorf("Classify(devnet fork id) = %q, want devnet", got)
	}
	if got := ClassifyStatus(3151909, nw.GenesisHash()); got != "devnet" {
		t.Errorf("ClassifyStatus(devnet network id) = %q, want devnet", got)
	}
	if got := ClassifyStatus(3151908, nw.GenesisHash()); got != "" {
		t.Errorf("chain id incorrectly accepted as network id: %q", got)
	}
	if got := ClassifyCL([4]byte{0xaa, 0x49, 0xa4, 0x38}); got != "devnet" {
		t.Errorf("ClassifyCL(0xaa49a438 Fusaka BPO digest) = %q, want devnet", got)
	}
	if !slices.Contains(Names(), "devnet") {
		t.Error("devnet missing from Names()")
	}
}

func TestRegisterDevnetIsSingular(t *testing.T) {
	registerDevnetForTest(t)
	if err := RegisterDevnet(devnetConfig()); err == nil {
		t.Fatal("second registration accepted; one process must not host two enclaves")
	}
}

func TestRegisterDevnetRejectsBadConfig(t *testing.T) {
	tests := []struct {
		name  string
		spoil func(*DevnetConfig)
	}{
		{"no network id", func(c *DevnetConfig) { c.NetworkID = 0 }},
		{"bad validators root", func(c *DevnetConfig) { c.GenesisValidatorsRoot = "0xdeadbeef" }},
		{"no bootnodes", func(c *DevnetConfig) { c.BootnodeRecords = nil }},
		{"no cl forks", func(c *DevnetConfig) { c.CLForks = nil }},
		{"no genesis config", func(c *DevnetConfig) { c.ELGenesisJSON = []byte(`{}`) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := devnetConfig()
			tt.spoil(&cfg)
			if err := RegisterDevnet(cfg); err == nil {
				t.Fatal("invalid devnet config accepted")
			}
			if _, err := Get("devnet"); err == nil {
				t.Fatal("rejected config still registered the network")
			}
		})
	}
}
