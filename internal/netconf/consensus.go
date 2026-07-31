package netconf

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"
)

type blobParams struct {
	epoch    uint64
	maxBlobs uint64
}

type clFork struct {
	epoch   uint64
	version string
}

type clNetwork struct {
	name           string
	gvr            string
	genesisTime    uint64
	secondsPerSlot uint64
	slotsPerEpoch  uint64
	forks          []clFork
	fuluEpoch      uint64
	blobSchedule   []blobParams

	once    sync.Once
	digests map[[4]byte]struct{}

	stateMu    sync.RWMutex
	stateEpoch uint64
	state      CLForkState
	stateOK    bool
}

// CLForkState is the consensus networking state active at a wall-clock instant.
// ENRForkID's next fields describe only regular forks; NextDigest also covers BPOs.
type CLForkState struct {
	Digest          [4]byte
	CurrentVersion  [4]byte
	NextForkVersion [4]byte
	NextForkEpoch   uint64
	NextDigest      [4]byte
	NextTransition  time.Time
}

func forkDataRoot(version []byte, gvr []byte) []byte {
	var leaf [32]byte
	copy(leaf[:], version)
	h := sha256.New()
	h.Write(leaf[:])
	h.Write(gvr)
	return h.Sum(nil)
}

func decodeVersion(raw string) ([4]byte, error) {
	var out [4]byte
	b, err := hex.DecodeString(strings.TrimPrefix(strings.TrimSpace(raw), "0x"))
	if err != nil || len(b) != len(out) {
		return out, fmt.Errorf("invalid fork version %q", raw)
	}
	copy(out[:], b)
	return out, nil
}

func (c *clNetwork) epochAt(at time.Time) uint64 {
	unix := at.Unix()
	if unix <= 0 || uint64(unix) <= c.genesisTime {
		return 0
	}
	secondsPerEpoch, ok := c.epochDuration()
	if !ok {
		return 0
	}
	return (uint64(unix) - c.genesisTime) / secondsPerEpoch
}

func (c *clNetwork) timeAtEpoch(epoch uint64) time.Time {
	secondsPerEpoch, ok := c.epochDuration()
	maxUnix := uint64(math.MaxInt64)
	if !ok || c.genesisTime > maxUnix || epoch > (maxUnix-c.genesisTime)/secondsPerEpoch {
		return time.Unix(math.MaxInt64, 0).UTC()
	}
	return time.Unix(int64(c.genesisTime+epoch*secondsPerEpoch), 0).UTC()
}

func (c *clNetwork) epochDuration() (uint64, bool) {
	if c.secondsPerSlot == 0 || c.slotsPerEpoch > math.MaxUint64/c.secondsPerSlot {
		return 0, false
	}
	return c.secondsPerSlot * c.slotsPerEpoch, true
}

func (c *clNetwork) forkAt(epoch uint64) (clFork, error) {
	if len(c.forks) == 0 {
		return clFork{}, fmt.Errorf("network %q has no consensus fork schedule", c.name)
	}
	active := c.forks[0]
	for _, f := range c.forks[1:] {
		if f.epoch > epoch {
			break
		}
		active = f
	}
	return active, nil
}

func (c *clNetwork) blobAt(epoch uint64) (blobParams, error) {
	var active blobParams
	found := false
	for _, bp := range c.blobSchedule {
		if bp.epoch > epoch {
			break
		}
		active, found = bp, true
	}
	if !found {
		return blobParams{}, fmt.Errorf("network %q has no blob parameters for epoch %d", c.name, epoch)
	}
	return active, nil
}

func (c *clNetwork) rawDigest(version [4]byte) ([4]byte, error) {
	var digest [4]byte
	gvr, err := hex.DecodeString(c.gvr)
	if err != nil || len(gvr) != 32 {
		return digest, fmt.Errorf("network %q has invalid genesis validators root", c.name)
	}
	base := forkDataRoot(version[:], gvr)
	copy(digest[:], base[:4])
	return digest, nil
}

// digestAt implements compute_fork_digest, including EIP-7892 masking from Fulu.
func (c *clNetwork) digestAt(epoch uint64) ([4]byte, [4]byte, error) {
	var digest [4]byte
	fork, err := c.forkAt(epoch)
	if err != nil {
		return digest, [4]byte{}, err
	}
	version, err := decodeVersion(fork.version)
	if err != nil {
		return digest, [4]byte{}, err
	}
	digest, err = c.rawDigest(version)
	if err != nil {
		return digest, version, err
	}
	if c.fuluEpoch != math.MaxUint64 && epoch >= c.fuluEpoch {
		bp, err := c.blobAt(epoch)
		if err != nil {
			return digest, version, err
		}
		var pre [16]byte
		binary.LittleEndian.PutUint64(pre[0:8], bp.epoch)
		binary.LittleEndian.PutUint64(pre[8:16], bp.maxBlobs)
		mask := sha256.Sum256(pre[:])
		for i := range digest {
			digest[i] ^= mask[i]
		}
	}
	return digest, version, nil
}

// The state is a pure function of the epoch, so it is cached per epoch: this runs per
// row for every CL node and otherwise recomputes SHA-256 fork digests each time.
func (c *clNetwork) stateAt(at time.Time) (CLForkState, error) {
	epoch := c.epochAt(at)
	c.stateMu.RLock()
	if c.stateOK && c.stateEpoch == epoch {
		state := c.state
		c.stateMu.RUnlock()
		return state, nil
	}
	c.stateMu.RUnlock()
	state, err := c.computeStateAt(epoch)
	if err != nil {
		return CLForkState{}, err
	}
	c.stateMu.Lock()
	c.stateEpoch, c.state, c.stateOK = epoch, state, true
	c.stateMu.Unlock()
	return state, nil
}

func (c *clNetwork) computeStateAt(epoch uint64) (CLForkState, error) {
	digest, version, err := c.digestAt(epoch)
	if err != nil {
		return CLForkState{}, err
	}
	state := CLForkState{
		Digest:          digest,
		CurrentVersion:  version,
		NextForkVersion: version,
		NextForkEpoch:   math.MaxUint64,
	}

	var nextEpoch uint64 = math.MaxUint64
	for _, fork := range c.forks {
		if fork.epoch <= epoch {
			continue
		}
		state.NextForkEpoch = fork.epoch
		state.NextForkVersion, err = decodeVersion(fork.version)
		if err != nil {
			return CLForkState{}, err
		}
		nextEpoch = fork.epoch
		break
	}
	for _, bp := range c.blobSchedule {
		if bp.epoch > epoch && bp.epoch < nextEpoch {
			nextEpoch = bp.epoch
			break
		}
	}
	if nextEpoch != math.MaxUint64 {
		state.NextDigest, _, err = c.digestAt(nextEpoch)
		if err != nil {
			return CLForkState{}, err
		}
		state.NextTransition = c.timeAtEpoch(nextEpoch)
	}
	return state, nil
}

func (c *clNetwork) compute() {
	c.once.Do(func() {
		c.digests = make(map[[4]byte]struct{}, len(c.forks)+len(c.blobSchedule))
		for _, fork := range c.forks {
			version, err := decodeVersion(fork.version)
			if err != nil {
				continue
			}
			var digest [4]byte
			if c.fuluEpoch == math.MaxUint64 || fork.epoch < c.fuluEpoch {
				digest, err = c.rawDigest(version)
			} else {
				digest, _, err = c.digestAt(fork.epoch)
			}
			if err == nil {
				c.digests[digest] = struct{}{}
			}
		}
		for _, bp := range c.blobSchedule {
			if bp.epoch < c.fuluEpoch {
				continue
			}
			digest, _, err := c.digestAt(bp.epoch)
			if err == nil {
				c.digests[digest] = struct{}{}
			}
		}
	})
}

func clNetworkByName(name string) (*clNetwork, error) {
	for _, c := range clNetworks {
		if c.name == name {
			return c, nil
		}
	}
	return nil, fmt.Errorf("unknown consensus network %q", name)
}

func CLForkStateAt(name string, at time.Time) (CLForkState, error) {
	c, err := clNetworkByName(name)
	if err != nil {
		return CLForkState{}, err
	}
	return c.stateAt(at)
}

func IsCurrentCLForkAt(name, forkHash string, at time.Time) bool {
	raw, err := hex.DecodeString(strings.TrimPrefix(strings.ToLower(strings.TrimSpace(forkHash)), "0x"))
	if err != nil || len(raw) != 4 {
		return false
	}
	state, err := CLForkStateAt(name, at)
	return err == nil && string(raw) == string(state.Digest[:])
}

// blobSchedule begins with the Electra fallback parameters used from Fulu until the
// first explicit BPO entry.
var clNetworks = []*clNetwork{
	{
		name: "mainnet", gvr: "4b363db94e286120d76eb905340fdd4e54bfe9f06bf33ff6cf5ad27f511bfe95",
		genesisTime: 1606824023, secondsPerSlot: 12, slotsPerEpoch: 32, fuluEpoch: 411392,
		forks:        []clFork{{0, "00000000"}, {74240, "01000000"}, {144896, "02000000"}, {194048, "03000000"}, {269568, "04000000"}, {364032, "05000000"}, {411392, "06000000"}},
		blobSchedule: []blobParams{{364032, 9}, {412672, 15}, {419072, 21}},
	},
	{
		name: "hoodi", gvr: "212f13fc4df078b6cb7db228f1c8307566dcecf900867401a92023d7ba99cb5f",
		genesisTime: 1742213400, secondsPerSlot: 12, slotsPerEpoch: 32, fuluEpoch: 50688,
		forks:        []clFork{{0, "10000910"}, {0, "20000910"}, {0, "30000910"}, {0, "40000910"}, {0, "50000910"}, {2048, "60000910"}, {50688, "70000910"}},
		blobSchedule: []blobParams{{2048, 9}, {52480, 15}, {54016, 21}},
	},
	{
		name: "sepolia", gvr: "d8ea171f3c94aea21ebc42a1ed61052acf3f9209c00e4efbaaddac09ed9b8078",
		genesisTime: 1655733600, secondsPerSlot: 12, slotsPerEpoch: 32, fuluEpoch: 272640,
		forks:        []clFork{{0, "90000069"}, {50, "90000070"}, {100, "90000071"}, {56832, "90000072"}, {132608, "90000073"}, {222464, "90000074"}, {272640, "90000075"}},
		blobSchedule: []blobParams{{222464, 9}, {274176, 15}, {275712, 21}},
	},
}

func ClassifyCL(forkDigest [4]byte) string {
	for _, c := range clNetworks {
		c.compute()
		if _, ok := c.digests[forkDigest]; ok {
			return c.name
		}
	}
	return ""
}

func CurrentCLForkENRAt(name string, at time.Time) ([]byte, error) {
	state, err := CLForkStateAt(name, at)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 16)
	copy(out[:4], state.Digest[:])
	copy(out[4:8], state.NextForkVersion[:])
	binary.LittleEndian.PutUint64(out[8:16], state.NextForkEpoch)
	return out, nil
}

func CurrentCLForkENR(name string) ([]byte, error) {
	return CurrentCLForkENRAt(name, time.Now())
}

func CurrentCLNFDAt(name string, at time.Time) ([]byte, error) {
	state, err := CLForkStateAt(name, at)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 4)
	copy(out, state.NextDigest[:])
	return out, nil
}

func CurrentCLNFD(name string) ([]byte, error) {
	return CurrentCLNFDAt(name, time.Now())
}
