package netconf

import (
	"cmp"
	"encoding/binary"
	"hash/crc32"
	"math/big"
	"reflect"
	"slices"
	"strings"

	"github.com/ethereum/go-ethereum/core/forkid"
	"github.com/ethereum/go-ethereum/params"
)

// forkIDAt computes the EIP-2124 fork id from this package's fork list instead of calling
// forkid.NewID, which reads only params.ChainConfig fields: a fork the pinned go-ethereum cannot
// name yet (an override or a devnet genesis key) must still enter the checksum and Next.
func (n *Network) forkIDAt(at uint64) forkid.ID {
	n.load()
	sum := crc32.ChecksumIEEE(n.genesis.Hash().Bytes())
	for _, fork := range n.idBlocks {
		if fork > forkHeadBlock {
			return forkid.ID{Hash: checksumBytes(sum), Next: fork}
		}
		sum = crc32.Update(sum, crc32.IEEETable, binary.BigEndian.AppendUint64(nil, fork))
	}
	for _, fork := range n.idTimes {
		if fork > at {
			return forkid.ID{Hash: checksumBytes(sum), Next: fork}
		}
		sum = crc32.Update(sum, crc32.IEEETable, binary.BigEndian.AppendUint64(nil, fork))
	}
	return forkid.ID{Hash: checksumBytes(sum)}
}

func checksumBytes(sum uint32) [4]byte {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], sum)
	return b
}

// forkBlocks lists the block-numbered forks of cfg. Block 0 is the genesis ruleset, not a fork. Every
// value goes through big.Int.Uint64, as in go-ethereum, so even a nonsensical negative block matches it.
func forkBlocks(cfg *params.ChainConfig) []uint64 {
	v := reflect.ValueOf(cfg).Elem()
	t := v.Type()
	var out []uint64
	for i := 0; i < t.NumField(); i++ {
		if !strings.HasSuffix(t.Field(i).Name, "Block") {
			continue
		}
		if b, ok := v.Field(i).Interface().(*big.Int); ok && b != nil {
			out = append(out, b.Uint64())
		}
	}
	slices.Sort(out)
	out = slices.Compact(out)
	if len(out) > 0 && out[0] == 0 {
		out = out[1:]
	}
	return out
}

// idForkTimes lists the distinct activation times after genesis, the time half of EIP-2124's fork
// list. A time at or before genesis is part of the genesis ruleset.
func idForkTimes(forks []elForkTime, genesis uint64) []uint64 {
	var out []uint64
	for _, f := range forks {
		if f.time > genesis {
			out = append(out, f.time)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// mergeELForks orders go-ethereum's forks and the extra ones by time. Extras sort by name first, and the
// sort is stable, so every process gives a tie the same last fork, which names the era and its history key.
func mergeELForks(geth []elForkTime, extras ...[]elForkTime) []elForkTime {
	var extra []elForkTime
	for _, e := range extras {
		extra = append(extra, e...)
	}
	slices.SortFunc(extra, func(a, b elForkTime) int {
		if a.time != b.time {
			return cmp.Compare(a.time, b.time)
		}
		return strings.Compare(a.name, b.name)
	})
	out := append(slices.Clone(geth), extra...)
	slices.SortStableFunc(out, func(a, b elForkTime) int { return cmp.Compare(a.time, b.time) })
	return out
}
