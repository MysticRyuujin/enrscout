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
	version [4]byte
}

type clNetwork struct {
	name           string
	gvr            [32]byte
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

func (c *clNetwork) rawDigest(version [4]byte) [4]byte {
	var digest [4]byte
	copy(digest[:], forkDataRoot(version[:], c.gvr[:]))
	return digest
}

// digestAt implements compute_fork_digest, including EIP-7892 masking from Fulu.
func (c *clNetwork) digestAt(epoch uint64) ([4]byte, [4]byte, error) {
	var digest [4]byte
	fork, err := c.forkAt(epoch)
	if err != nil {
		return digest, [4]byte{}, err
	}
	version := fork.version
	digest = c.rawDigest(version)
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
		state.NextForkVersion = fork.version
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
			if c.fuluEpoch == math.MaxUint64 || fork.epoch < c.fuluEpoch {
				c.digests[c.rawDigest(fork.version)] = struct{}{}
			} else if digest, _, err := c.digestAt(fork.epoch); err == nil {
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

func CLForkStateAt(name string, at time.Time) (CLForkState, error) {
	n, err := Get(name)
	if err != nil || n.cl == nil {
		return CLForkState{}, fmt.Errorf("unknown consensus network %q", name)
	}
	return n.cl.stateAt(at)
}

func IsCurrentCLForkAt(name, forkHash string, at time.Time) bool {
	digest, ok := parseHash4(forkHash)
	if !ok {
		return false
	}
	state, err := CLForkStateAt(name, at)
	return err == nil && digest == state.Digest
}

type namedFork struct {
	name    string
	epoch   uint64
	version string
}

// electraMaxBlobs is MAX_BLOBS_PER_BLOCK_ELECTRA, the blob limit every built-in network uses from
// Electra until its first BPO.
const electraMaxBlobs = 9

// compiledCL builds a built-in network. Fulu's epoch and the Electra fallback blob entry (used from
// Fulu until the first BPO) are derived from the named forks rather than repeated beside them.
func compiledCL(name, gvr string, genesisTime uint64, forks []namedFork, bpos []blobParams) *clNetwork {
	root, err := hex.DecodeString(gvr)
	if err != nil || len(root) != 32 {
		panic(fmt.Sprintf("netconf: %s genesis validators root %q", name, gvr))
	}
	c := &clNetwork{name: name, genesisTime: genesisTime, secondsPerSlot: 12, slotsPerEpoch: 32, fuluEpoch: math.MaxUint64}
	copy(c.gvr[:], root)
	for _, f := range forks {
		version, err := decodeVersion(f.version)
		if err != nil {
			panic(fmt.Sprintf("netconf: %s %s: %v", name, f.name, err))
		}
		c.forks = append(c.forks, clFork{epoch: f.epoch, version: version})
		switch f.name {
		case "electra":
			c.blobSchedule = append(c.blobSchedule, blobParams{f.epoch, electraMaxBlobs})
		case "fulu":
			c.fuluEpoch = f.epoch
		}
	}
	c.blobSchedule = append(c.blobSchedule, bpos...)
	return c
}

var (
	mainnetCL = compiledCL("mainnet", "4b363db94e286120d76eb905340fdd4e54bfe9f06bf33ff6cf5ad27f511bfe95", 1606824023,
		[]namedFork{{"phase0", 0, "00000000"}, {"altair", 74240, "01000000"}, {"bellatrix", 144896, "02000000"}, {"capella", 194048, "03000000"},
			{"deneb", 269568, "04000000"}, {"electra", 364032, "05000000"}, {"fulu", 411392, "06000000"}},
		[]blobParams{{412672, 15}, {419072, 21}})
	hoodiCL = compiledCL("hoodi", "212f13fc4df078b6cb7db228f1c8307566dcecf900867401a92023d7ba99cb5f", 1742213400,
		[]namedFork{{"phase0", 0, "10000910"}, {"altair", 0, "20000910"}, {"bellatrix", 0, "30000910"}, {"capella", 0, "40000910"},
			{"deneb", 0, "50000910"}, {"electra", 2048, "60000910"}, {"fulu", 50688, "70000910"}},
		[]blobParams{{52480, 15}, {54016, 21}})
	sepoliaCL = compiledCL("sepolia", "d8ea171f3c94aea21ebc42a1ed61052acf3f9209c00e4efbaaddac09ed9b8078", 1655733600,
		[]namedFork{{"phase0", 0, "90000069"}, {"altair", 50, "90000070"}, {"bellatrix", 100, "90000071"}, {"capella", 56832, "90000072"},
			{"deneb", 132608, "90000073"}, {"electra", 222464, "90000074"}, {"fulu", 272640, "90000075"}, {"gloas", 353024, "90000076"}},
		[]blobParams{{274176, 15}, {275712, 21}})
)

func ClassifyCL(forkDigest [4]byte) string {
	for _, n := range registry {
		if n.cl == nil {
			continue
		}
		n.cl.compute()
		if _, ok := n.cl.digests[forkDigest]; ok {
			return n.Name
		}
	}
	return ""
}

// ENRForkID is the SSZ ENRForkID an eth2 ENR entry carries: digest, next fork version, and the
// little-endian next fork epoch.
func (s CLForkState) ENRForkID() []byte {
	out := make([]byte, 16)
	copy(out[:4], s.Digest[:])
	copy(out[4:8], s.NextForkVersion[:])
	binary.LittleEndian.PutUint64(out[8:16], s.NextForkEpoch)
	return out
}
