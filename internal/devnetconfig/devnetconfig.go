// Package devnetconfig loads the file bundle used to register one custom
// execution/consensus devnet in crawler and API processes.
package devnetconfig

import (
	"bufio"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/MysticRyuujin/enrscout/internal/netconf"
)

// Load reads a crawler/API devnet directory without mutating netconf's global
// registry. Call netconf.RegisterDevnet before starting concurrent users.
func Load(dir string) (netconf.DevnetConfig, error) {
	var cfg netconf.DevnetConfig

	genesis, err := os.ReadFile(filepath.Join(dir, "genesis.json"))
	if err != nil {
		return cfg, fmt.Errorf("read genesis.json: %w", err)
	}
	cfg.ELGenesisJSON = genesis
	networkIDRaw, err := os.ReadFile(filepath.Join(dir, "network_id.txt"))
	if err != nil {
		return cfg, fmt.Errorf("read network_id.txt: %w", err)
	}
	cfg.NetworkID, err = strconv.ParseUint(strings.TrimSpace(string(networkIDRaw)), 0, 64)
	if err != nil || cfg.NetworkID == 0 {
		return cfg, fmt.Errorf("parse network_id.txt: expected a non-zero uint64")
	}

	gvr, err := os.ReadFile(filepath.Join(dir, "genesis_validators_root.txt"))
	if err != nil {
		return cfg, fmt.Errorf("read genesis_validators_root.txt: %w", err)
	}
	cfg.GenesisValidatorsRoot = strings.TrimSpace(string(gvr))

	cl, err := parseCLConfig(filepath.Join(dir, "config.yaml"))
	if err != nil {
		return cfg, err
	}
	cfg.GenesisTime = cl.genesisTime
	cfg.SecondsPerSlot = cl.secondsPerSlot
	cfg.SlotsPerEpoch = cl.slotsPerEpoch
	cfg.CLForks = cl.forks
	cfg.FuluForkEpoch = cl.fuluEpoch
	if cl.fuluActive {
		cfg.BlobSchedule = append([]netconf.BlobParams{{Epoch: cl.electraEpoch, MaxBlobs: cl.electraMaxBlobs}}, cl.blobSchedule...)
	}

	records, err := readLines(filepath.Join(dir, "bootnodes.txt"))
	if err != nil {
		return cfg, fmt.Errorf("read bootnodes.txt: %w", err)
	}
	cfg.BootnodeRecords = records
	return cfg, nil
}

type clConfig struct {
	forks           []netconf.CLForkConfig
	fuluVersion     string
	fuluEpoch       uint64
	fuluActive      bool
	blobSchedule    []netconf.BlobParams
	electraEpoch    uint64
	electraMaxBlobs uint64
	genesisTime     uint64
	secondsPerSlot  uint64
	slotDurationMS  uint64
	slotsPerEpoch   uint64
}

func parseCLConfig(path string) (clConfig, error) {
	cfg := clConfig{electraMaxBlobs: 9, fuluEpoch: math.MaxUint64}
	f, err := os.Open(path)
	if err != nil {
		return cfg, fmt.Errorf("read config.yaml: %w", err)
	}
	defer f.Close()

	versions := map[string]string{}
	epochs := map[string]uint64{"GENESIS": 0}
	var forkOrder []string
	inBlob := false
	var pendEpoch, pendMax *uint64
	flush := func() error {
		if pendEpoch == nil && pendMax == nil {
			return nil
		}
		if pendEpoch == nil || pendMax == nil {
			return fmt.Errorf("incomplete BLOB_SCHEDULE entry: both EPOCH and MAX_BLOBS_PER_BLOCK are required")
		}
		cfg.blobSchedule = append(cfg.blobSchedule, netconf.BlobParams{Epoch: *pendEpoch, MaxBlobs: *pendMax})
		pendEpoch, pendMax = nil, nil
		return nil
	}

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		raw := sc.Text()
		line := clean(raw)
		if line == "" {
			continue
		}
		if line == "BLOB_SCHEDULE:" {
			inBlob = true
			continue
		}
		if inBlob {
			if indented := len(raw) > 0 && (raw[0] == ' ' || raw[0] == '\t' || raw[0] == '-'); !indented {
				inBlob = false
				if err := flush(); err != nil {
					return cfg, err
				}
			} else {
				trimmed := strings.TrimSpace(line)
				if strings.HasPrefix(trimmed, "-") {
					if err := flush(); err != nil {
						return cfg, err
					}
				}
				entry := strings.TrimSpace(strings.TrimPrefix(trimmed, "-"))
				if k, v, ok := strings.Cut(entry, ":"); ok {
					key := strings.TrimSpace(k)
					if key == "EPOCH" || key == "MAX_BLOBS_PER_BLOCK" {
						n, err := strconv.ParseUint(clean(v), 10, 64)
						if err != nil {
							return cfg, fmt.Errorf("invalid BLOB_SCHEDULE %s: %w", key, err)
						}
						if key == "EPOCH" {
							pendEpoch = &n
						} else {
							pendMax = &n
						}
					}
				}
				continue
			}
		}

		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = clean(val)
		switch {
		case strings.HasSuffix(key, "_FORK_VERSION"):
			if val != "" {
				name := strings.TrimSuffix(key, "_FORK_VERSION")
				versions[name] = val
				forkOrder = append(forkOrder, name)
			}
			if key == "FULU_FORK_VERSION" {
				cfg.fuluVersion = val
			}
		case strings.HasSuffix(key, "_FORK_EPOCH"):
			n, err := strconv.ParseUint(val, 10, 64)
			if err != nil {
				return cfg, fmt.Errorf("invalid %s: %w", key, err)
			}
			name := strings.TrimSuffix(key, "_FORK_EPOCH")
			epochs[name] = n
			if name == "FULU" {
				cfg.fuluEpoch = n
			}
			if name == "ELECTRA" {
				cfg.electraEpoch = n
			}
		case key == "GENESIS_TIME":
			n, err := strconv.ParseUint(val, 10, 64)
			if err != nil {
				return cfg, fmt.Errorf("invalid %s: %w", key, err)
			}
			cfg.genesisTime = n
		case key == "SECONDS_PER_SLOT":
			n, err := strconv.ParseUint(val, 10, 64)
			if err != nil {
				return cfg, fmt.Errorf("invalid SECONDS_PER_SLOT: %w", err)
			}
			cfg.secondsPerSlot = n
		case key == "SLOT_DURATION_MS":
			n, err := strconv.ParseUint(val, 10, 64)
			if err != nil {
				return cfg, fmt.Errorf("invalid SLOT_DURATION_MS: %w", err)
			}
			cfg.slotDurationMS = n
		case key == "SLOTS_PER_EPOCH":
			n, err := strconv.ParseUint(val, 10, 64)
			if err != nil {
				return cfg, fmt.Errorf("invalid SLOTS_PER_EPOCH: %w", err)
			}
			cfg.slotsPerEpoch = n
		case key == "PRESET_BASE":
			switch val {
			case "mainnet":
				cfg.slotsPerEpoch = 32
			case "minimal":
				cfg.slotsPerEpoch = 8
			}
		case key == "MAX_BLOBS_PER_BLOCK_ELECTRA":
			n, err := strconv.ParseUint(val, 10, 64)
			if err != nil {
				return cfg, fmt.Errorf("invalid MAX_BLOBS_PER_BLOCK_ELECTRA: %w", err)
			}
			cfg.electraMaxBlobs = n
		}
	}
	if err := flush(); err != nil {
		return cfg, err
	}
	if err := sc.Err(); err != nil {
		return cfg, fmt.Errorf("scan config.yaml: %w", err)
	}
	for _, name := range forkOrder {
		epoch, ok := epochs[name]
		if !ok {
			epoch = math.MaxUint64
		}
		cfg.forks = append(cfg.forks, netconf.CLForkConfig{Epoch: epoch, Version: versions[name]})
	}
	cfg.fuluActive = cfg.fuluEpoch != math.MaxUint64 && cfg.fuluVersion != ""
	// Newer ethereum-package genesis bundles publish SLOT_DURATION_MS instead of
	// SECONDS_PER_SLOT; an explicit SECONDS_PER_SLOT wins when both are present.
	if cfg.secondsPerSlot == 0 && cfg.slotDurationMS != 0 {
		if cfg.slotDurationMS%1000 != 0 {
			return cfg, fmt.Errorf("SLOT_DURATION_MS %d is not a whole number of seconds", cfg.slotDurationMS)
		}
		cfg.secondsPerSlot = cfg.slotDurationMS / 1000
	}
	if cfg.genesisTime == 0 || cfg.secondsPerSlot == 0 || cfg.slotsPerEpoch == 0 {
		return cfg, fmt.Errorf("config.yaml must provide genesis time, seconds per slot, and slots per epoch/preset")
	}
	return cfg, nil
}

func clean(s string) string {
	if i := strings.IndexByte(s, '#'); i >= 0 {
		s = s[:i]
	}
	return strings.Trim(strings.TrimSpace(s), `"'`)
}

func readLines(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var out []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, line)
	}
	return out, sc.Err()
}
