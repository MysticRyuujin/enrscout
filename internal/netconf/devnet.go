package netconf

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/p2p/enode"
)

// DevnetConfig defines a devnet registered at startup; its fork id/digest are per-enclave, so it can't be compiled in.
type DevnetConfig struct {
	ELGenesisJSON         []byte
	NetworkID             uint64
	GenesisValidatorsRoot string
	GenesisTime           uint64
	SecondsPerSlot        uint64
	SlotsPerEpoch         uint64
	CLForks               []CLForkConfig
	FuluForkEpoch         uint64
	BlobSchedule          []BlobParams
	BootnodeRecords       []string
}

type CLForkConfig struct {
	Name    string
	Epoch   uint64
	Version string
}

type BlobParams struct {
	Epoch    uint64
	MaxBlobs uint64
}

func RegisterDevnet(cfg DevnetConfig) error {
	const name = "devnet"
	if _, err := Get(name); err == nil {
		return fmt.Errorf("network %q already registered", name)
	}

	var g core.Genesis
	if err := json.Unmarshal(cfg.ELGenesisJSON, &g); err != nil {
		return fmt.Errorf("parse EL genesis.json: %w", err)
	}
	if g.Config == nil {
		return fmt.Errorf("EL genesis.json has no chain config")
	}
	if cfg.NetworkID == 0 {
		return fmt.Errorf("EL network ID must be non-zero and supplied separately from chain ID")
	}
	genesis := g

	root, err := hex.DecodeString(strings.TrimPrefix(strings.TrimSpace(cfg.GenesisValidatorsRoot), "0x"))
	if err != nil || len(root) != 32 {
		return fmt.Errorf("invalid genesis validators root %q", cfg.GenesisValidatorsRoot)
	}
	var gvr [32]byte
	copy(gvr[:], root)

	records := make([]string, 0, len(cfg.BootnodeRecords))
	for _, r := range cfg.BootnodeRecords {
		r = strings.TrimSpace(r)
		if r == "" {
			continue
		}
		if _, err := enode.Parse(enode.ValidSchemes, r); err != nil {
			continue
		}
		records = append(records, r)
	}
	if len(records) == 0 {
		return fmt.Errorf("no valid devnet bootnode records")
	}

	if cfg.GenesisTime == 0 || cfg.SecondsPerSlot == 0 || cfg.SlotsPerEpoch == 0 {
		return fmt.Errorf("CL genesis time, seconds per slot, and slots per epoch must be non-zero")
	}
	if cfg.GenesisTime > uint64(math.MaxInt64) {
		return fmt.Errorf("CL genesis time exceeds the maximum Unix timestamp")
	}
	if cfg.SlotsPerEpoch > math.MaxUint64/cfg.SecondsPerSlot {
		return fmt.Errorf("CL epoch duration overflows uint64")
	}
	forks := make([]clFork, 0, len(cfg.CLForks))
	for _, configured := range cfg.CLForks {
		fv := strings.TrimPrefix(strings.TrimSpace(configured.Version), "0x")
		if fv == "" || configured.Epoch == math.MaxUint64 {
			continue
		}
		version, err := decodeVersion(fv)
		if err != nil {
			return fmt.Errorf("invalid CL fork version %q", fv)
		}
		forks = append(forks, clFork{name: strings.ToLower(configured.Name), epoch: configured.Epoch, version: version})
	}
	if len(forks) == 0 {
		return fmt.Errorf("no CL fork versions")
	}
	sort.SliceStable(forks, func(i, j int) bool { return forks[i].epoch < forks[j].epoch })

	bs := make([]blobParams, len(cfg.BlobSchedule))
	for i, b := range cfg.BlobSchedule {
		bs[i] = blobParams{epoch: b.Epoch, maxBlobs: b.MaxBlobs}
	}
	sort.SliceStable(bs, func(i, j int) bool { return bs[i].epoch < bs[j].epoch })
	if cfg.FuluForkEpoch != math.MaxUint64 && len(bs) == 0 {
		return errors.New("blob schedule is empty but a Fulu fork version is set")
	}

	registry = append(registry, &Network{
		Name:        name,
		NetworkID:   cfg.NetworkID,
		ChainConfig: genesis.Config,
		genesisFn:   func() *core.Genesis { return &genesis },
		bootnodes:   records,
		clBootnodes: records,
		cl: &clNetwork{
			name:           name,
			gvr:            gvr,
			genesisTime:    cfg.GenesisTime,
			secondsPerSlot: cfg.SecondsPerSlot,
			slotsPerEpoch:  cfg.SlotsPerEpoch,
			forks:          forks,
			fuluEpoch:      cfg.FuluForkEpoch,
			blobSchedule:   bs,
		},
	})
	return nil
}
