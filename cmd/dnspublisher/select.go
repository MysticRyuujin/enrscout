package main

import (
	"encoding/hex"
	"errors"
	"math"
	"sort"
	"time"

	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/p2p/enr"
	"github.com/ethereum/go-ethereum/rlp"

	"github.com/MysticRyuujin/enrscout/internal/clientname"
	"github.com/MysticRyuujin/enrscout/internal/netconf"
	"github.com/MysticRyuujin/enrscout/internal/nodeset"
)

const (
	balanceProportional = "proportional"
	balanceNone         = "none"
	unknownClient       = "<unknown>"
)

type selectOpts struct {
	minScore   int
	maxAge     time.Duration
	protocol   string
	layer      string
	capability string
	limit      int
	balance    string
}

// capability is set per tree by buildNetworkTrees, so it is not validated here.
func (o selectOpts) Validate() error {
	if o.minScore < 0 || int64(o.minScore) > int64(1<<31-1) {
		return errors.New("--min-score must be between 0 and 2147483647")
	}
	if o.maxAge < 0 {
		return errors.New("--max-age must not be negative")
	}
	if o.protocol != "any" && o.protocol != "v4" && o.protocol != "v5" {
		return errors.New("--protocol must be any, v4, or v5")
	}
	if o.layer != "el" && o.layer != "cl" && o.layer != "any" {
		return errors.New("--layer must be el, cl, or any")
	}
	if o.limit < 0 {
		return errors.New("--limit must not be negative")
	}
	if o.balance != balanceProportional && o.balance != balanceNone {
		return errors.New("--client-balance must be proportional or none")
	}
	return nil
}

// enrForkCurrentAt evaluates the row currency rule on the ENR the tree will carry: a Status-classified
// row can be current while its ENR advertises no fork entry or a stale one, and consumers pre-qualify
// peers from the record alone. Same bar as discv4-crawl's `devp2p nodeset filter -eth-network`.
func enrForkCurrentAt(n *enode.Node, layer, network string, now time.Time) bool {
	switch layer {
	case "el":
		var eth netconf.EthEntry
		if n.Record().Load(&eth) != nil {
			return false
		}
		return netconf.RowForkCurrentAt(layer, network, hex.EncodeToString(eth.ForkID.Hash[:]), eth.ForkID.Next, now)
	case "cl":
		// SSZ ENRForkID is fixed 16 bytes: a truncated entry can carry a current digest yet still fail strict consumers.
		var eth2 netconf.Eth2Entry
		if n.Record().Load(&eth2) != nil || len(eth2) != 16 {
			return false
		}
		return netconf.RowForkCurrentAt(layer, network, hex.EncodeToString(eth2[:4]), 0, now)
	default:
		return false
	}
}

// enrHasEntry reports ENR-entry presence, matching devp2p nodeset filter's snap/les selection (presence, not fingerprinted caps).
func enrHasEntry(n *enode.Node, key string) bool {
	var raw rlp.RawValue
	return n.Record().Load(enr.WithEntry(key, &raw)) == nil
}

// enrEntryDecodes reports whether an entry is absent or decodable, never present-but-broken. The crawler
// stays tolerant of a node whose entry stops decoding (classification is sticky, `nodeset.Observe`), but a
// consumer may reject the whole record over one bad entry - ethrex does - so publishing it wastes a slot.
func enrEntryDecodes(n *enode.Node, entry enr.Entry) bool {
	err := n.Record().Load(entry)
	return err == nil || enr.IsNotFound(err)
}

// enrWellFormed checks the typed entries a peer decodes when it reads the record. A port above
// uint16 or a wrong-length address is not representable in the ENR-typed field, and go-ethereum
// reports it only when that one entry is loaded, so the record still signs and parses. Peers that
// decode entries strictly drop the whole record instead.
func enrWellFormed(n *enode.Node) bool {
	for _, entry := range []enr.Entry{
		new(netconf.EthEntry),
		new(enr.IPv4), new(enr.IPv6),
		new(enr.TCP), new(enr.TCP6),
		new(enr.UDP), new(enr.UDP6),
		new(enr.QUIC), new(enr.QUIC6),
	} {
		if !enrEntryDecodes(n, entry) {
			return false
		}
	}
	return true
}

// candidate is a row every tree of a network may publish, with its record parsed once.
type candidate struct {
	row    *nodeset.Row
	node   *enode.Node
	client string
	v6     ipv6Slot
}

// rankCandidates applies every per-row filter except capability, in rank order. It sorts rows in place.
func rankCandidates(rows []nodeset.Row, opt selectOpts, now time.Time) []candidate {
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Score != rows[j].Score {
			return rows[i].Score > rows[j].Score
		}
		if rows[i].LastSeen != rows[j].LastSeen {
			return rows[i].LastSeen > rows[j].LastSeen
		}
		return rows[i].ID < rows[j].ID
	})
	var out []candidate
	for i := range rows {
		r := &rows[i]
		if opt.layer != "" && opt.layer != "any" && r.Layer != opt.layer {
			continue
		}
		if !netconf.RowForkCurrentAt(r.Layer, r.Network, r.ForkHash, r.ForkNext, now) {
			continue
		}
		if !r.Dialable() {
			continue
		}
		if int(r.Score) < opt.minScore || r.ENR == "" {
			continue
		}
		if opt.maxAge > 0 && now.Sub(time.Unix(r.LastSeen, 0)) > opt.maxAge {
			continue
		}
		switch opt.protocol {
		case "v4":
			if !r.HasV4 {
				continue
			}
		case "v5":
			if !r.HasV5 {
				continue
			}
		}
		n, err := enode.Parse(enode.ValidSchemes, r.ENR)
		if err != nil {
			continue
		}
		if !enrWellFormed(n) {
			continue
		}
		if !enrForkCurrentAt(n, r.Layer, r.Network, now) {
			continue
		}
		out = append(out, candidate{
			row: r, node: n, client: clientBucket(*r),
			v6: ipv6Slot{dialable: r.DialableV6(), ownPort: r.TCP6 != 0 || r.QUIC6 != 0},
		})
	}
	return out
}

// pick selects one tree's nodes from the ranked candidates.
func pick(cands []candidate, opt selectOpts) []candidate {
	if opt.capability == "snap" {
		var snap []candidate
		for _, c := range cands {
			if enrHasEntry(c.node, "snap") {
				snap = append(snap, c)
			}
		}
		cands = snap
	}
	if opt.limit <= 0 || len(cands) <= opt.limit {
		return cands
	}
	v6 := make([]ipv6Slot, len(cands))
	for i, c := range cands {
		v6[i] = c.v6
	}
	reserved := reserveIPv6(v6, opt.limit)
	rest := make([]candidate, 0, len(cands))
	held := map[string]int{}
	for i, c := range cands {
		if reserved[i] {
			held[c.client]++
		} else {
			rest = append(rest, c)
		}
	}
	free := opt.limit - countTrue(reserved)
	var filled []candidate
	switch {
	case free <= 0:
	case opt.balance != balanceProportional || len(rest) <= free:
		filled = rest[:min(free, len(rest))]
	default:
		filled = balanceClients(rest, free, held)
	}
	return mergeRanked(cands, reserved, filled)
}

// ipv6Slot carries what the reservation needs about a candidate. ownPort marks an explicit tcp6/quic6:
// sigp's enr crate reads tcp6 with no fallback to tcp, so those records are the ones a discv5-based
// client can actually dial over v6, and they take the reserved slots first.
type ipv6Slot struct {
	dialable bool
	ownPort  bool
}

// reserveIPv6 marks the candidates that hold the tree's IPv6 slots.
//
// Address family is not a client-balance dimension, so on a limited tree a family holding a few
// percent of the pool rounds away to nothing: at limit=25 a 2.4% IPv6 share expects 0.6 nodes. This
// gives IPv6 its proportional share and never less than one slot, mirroring the per-client rule.
func reserveIPv6(v6 []ipv6Slot, limit int) []bool {
	reserved := make([]bool, len(v6))
	var candidates int
	for _, s := range v6 {
		if s.dialable {
			candidates++
		}
	}
	if candidates == 0 || limit <= 0 {
		return reserved
	}
	want := int(math.Round(float64(limit) * float64(candidates) / float64(len(v6))))
	want = min(max(want, 1), candidates, limit)

	taken := 0
	for _, ownPort := range []bool{true, false} {
		for i, s := range v6 {
			if taken == want {
				return reserved
			}
			if s.dialable && s.ownPort == ownPort && !reserved[i] {
				reserved[i] = true
				taken++
			}
		}
	}
	return reserved
}

func countTrue(b []bool) int {
	n := 0
	for _, v := range b {
		if v {
			n++
		}
	}
	return n
}

// mergeRanked restores candidate order, because tree layout depends on input order and a stable order
// keeps branch records identical between cycles that select the same nodes.
func mergeRanked(all []candidate, reserved []bool, filled []candidate) []candidate {
	keep := make(map[*enode.Node]bool, len(filled))
	for _, c := range filled {
		keep[c.node] = true
	}
	selected := make([]candidate, 0, countTrue(reserved)+len(filled))
	for i, c := range all {
		if reserved[i] || keep[c.node] {
			selected = append(selected, c)
		}
	}
	return selected
}

// Row.Client can come straight from a self-declared ENR entry, and even a completed handshake carries
// an attacker-chosen name, so a label only earns its own reserved slot when a verified fingerprint
// reports a client this repository recognizes. Everything else shares the unknown bucket, which the
// floor skips: otherwise a peer advertising many invented client names would take a whole small tree.
func clientBucket(r nodeset.Row) string {
	if r.FPStatus != "ok" && r.FPStatus != "stale" {
		return unknownClient
	}
	name := clientname.Canonical(r.Layer, r.Client)
	if !clientname.Recognized(name) {
		return unknownClient
	}
	return name
}

// balanceClients allocates the limit across clients rather than taking the highest-scoring nodes
// outright: score correlates with client, so an unbalanced tree drops whole clients from a list peers
// bootstrap against. Every identified client gets one slot, the rest are shared out in proportion to
// how much of the candidate pool each client holds. held names the buckets that already hold a
// reserved slot, so a client the IPv6 reservation already published does not also consume a floor
// slot and crowd out a client with none.
func balanceClients(cands []candidate, limit int, held map[string]int) []candidate {
	pool := map[string][]int{}
	var order []string
	for i, cand := range cands {
		c := cand.client
		if _, seen := pool[c]; !seen {
			order = append(order, c)
		}
		pool[c] = append(pool[c], i)
	}
	// Largest pool first, name breaking ties, so remainders are handed out deterministically and an
	// identical candidate set always yields an identical tree.
	sort.Slice(order, func(a, b int) bool {
		if len(pool[order[a]]) != len(pool[order[b]]) {
			return len(pool[order[a]]) > len(pool[order[b]])
		}
		return order[a] < order[b]
	})

	slots := make(map[string]int, len(order))
	remaining := limit
	for _, c := range order {
		if remaining == 0 {
			break
		}
		// "unknown" is the absence of a fingerprint, not a client worth reserving a slot for.
		if c == unknownClient || held[c] > 0 {
			continue
		}
		slots[c] = 1
		remaining--
	}
	free := 0
	for _, c := range order {
		free += len(pool[c]) - slots[c]
	}
	type remainder struct {
		client string
		frac   float64
	}
	var fracs []remainder
	for _, c := range order {
		if free <= 0 || remaining <= 0 {
			break
		}
		exact := float64(remaining) * float64(len(pool[c])-slots[c]) / float64(free)
		whole := min(int(exact), len(pool[c])-slots[c])
		slots[c] += whole
		fracs = append(fracs, remainder{c, exact - float64(whole)})
	}
	used := 0
	for _, c := range order {
		used += slots[c]
	}
	sort.Slice(fracs, func(a, b int) bool {
		if fracs[a].frac != fracs[b].frac {
			return fracs[a].frac > fracs[b].frac
		}
		return fracs[a].client < fracs[b].client
	})
	for i := 0; used < limit && len(fracs) > 0; i = (i + 1) % len(fracs) {
		c := fracs[i].client
		if slots[c] >= len(pool[c]) {
			if allFull(slots, pool, order) {
				break
			}
			continue
		}
		slots[c]++
		used++
	}

	var picked []int
	for _, c := range order {
		picked = append(picked, pool[c][:slots[c]]...)
	}
	// Restore the ranked order: tree layout depends on input order, so a stable order keeps branch
	// records identical between cycles that select the same nodes.
	sort.Ints(picked)
	selected := make([]candidate, 0, len(picked))
	for _, i := range picked {
		selected = append(selected, cands[i])
	}
	return selected
}

func allFull(slots map[string]int, pool map[string][]int, order []string) bool {
	for _, c := range order {
		if slots[c] < len(pool[c]) {
			return false
		}
	}
	return true
}
