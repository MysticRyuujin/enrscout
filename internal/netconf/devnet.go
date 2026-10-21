package netconf

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"reflect"
	"sort"
	"strings"

	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/params"
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
	extra, err := unknownELForks(cfg.ELGenesisJSON)
	if err != nil {
		return fmt.Errorf("parse EL genesis.json: %w", err)
	}

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

	for _, f := range extra {
		slog.Info("devnet EL fork unknown to go-ethereum", "fork", f.name, "time", f.time)
	}
	registry = append(registry, &Network{
		Name:         name,
		extraELForks: extra,
		NetworkID:    cfg.NetworkID,
		ChainConfig:  genesis.Config,
		genesisFn:    func() *core.Genesis { return &genesis },
		bootnodes:    records,
		clBootnodes:  records,
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

// unknownELForks reads the genesis config's <fork>Time keys that params.ChainConfig has no field
// for. core.Genesis drops them silently, which would leave the fork out of the devnet's fork id.
func unknownELForks(genesisJSON []byte) ([]elForkTime, error) {
	var raw struct {
		Config map[string]json.RawMessage `json:"config"`
	}
	if err := json.Unmarshal(genesisJSON, &raw); err != nil {
		return nil, err
	}
	known := map[string]bool{}
	t := reflect.TypeFor[params.ChainConfig]()
	for i := range t.NumField() {
		tag, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
		known[strings.ToLower(tag)] = true
	}
	var out []elForkTime
	for key, value := range raw.Config {
		name, ok := strings.CutSuffix(key, "Time")
		if !ok || name == "" || known[strings.ToLower(key)] || string(value) == "null" {
			continue
		}
		var at uint64
		if err := json.Unmarshal(value, &at); err != nil {
			return nil, fmt.Errorf("genesis config %s: %w", key, err)
		}
		if lower := strings.ToLower(name); strings.HasPrefix(lower, "bpo") {
			name = "BPO" + name[3:]
		} else {
			name = capitalize(name)
		}
		out = append(out, elForkTime{name: name, time: at})
	}
	return out, nil
}
