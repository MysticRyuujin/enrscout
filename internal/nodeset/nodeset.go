package nodeset

import (
	"bytes"
	"container/heap"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"net"
	"sort"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/core/forkid"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/p2p/enr"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/parquet-go/parquet-go"

	"github.com/MysticRyuujin/enrscout/internal/clientname"
	"github.com/MysticRyuujin/enrscout/internal/netconf"
	"github.com/MysticRyuujin/enrscout/internal/netpolicy"
)

const (
	scoreCap      = 10
	scoreInit     = 1
	dropBelow     = 0
	penaltyStep   = 2
	fpFailedAfter = 3
	// Client families and node identities are stable on real-world timescales;
	// daily revalidation keeps versions reasonably fresh without repeatedly
	// tripping peer limits and per-IP connection filters.
	fpRefreshAge = 24 * time.Hour
)

// Fingerprinting is deliberately much less aggressive than discovery. Peers may
// disconnect a crawler because it does not participate in gossip or serve useful
// protocol data, and retrying that transient rejection in a tight loop only makes
// the crawler's reputation worse. One and five minutes also clear common client
// recent-connection filters (roughly 30 seconds in Geth and five minutes in
// Nethermind). The final interval is reused for later failures.
var fpRetrySchedule = [...]time.Duration{
	1 * time.Minute,
	5 * time.Minute,
	15 * time.Minute,
	1 * time.Hour,
	3 * time.Hour,
	6 * time.Hour,
}

type Node struct {
	ID           enode.ID
	Enode        string
	ENR          string
	Seq          uint64
	IP           string
	IP6          string
	TCP          int
	UDP          int
	TCP6         int
	UDP6         int
	QUIC         int
	QUIC6        int
	Network      string
	ForkHash     string
	ForkNext     uint64
	Layer        string
	HasV4        bool
	HasV5        bool
	Score        int
	FirstSeen    time.Time
	LastSeen     time.Time
	LastCheck    time.Time
	LastResolved time.Time

	Client        string
	ClientVersion string
	OS            string
	Lang          string
	Capabilities  string
	// MembershipSource: "status" (live claim over authenticated Status) or "enr" (self-signed claim only); FPDirection: "outbound" | "inbound" of the last successful fingerprint.
	MembershipSource     string
	MembershipVerifiedAt time.Time
	ForkSource           string
	ForkObservedAt       time.Time
	FPDirection          string
	Country              string
	City                 string
	Subdivision          string
	Lat                  float64
	Lon                  float64
	ASN                  uint
	Org                  string
	Hosting              bool
	HostingKnown         bool
	Geolocated           bool
	GeoAccuracyRadiusKM  uint16

	fpAttempts int
	fpInFlight bool
	fpDone     bool
	// fpRefreshDue marks a retained successful fingerprint for background
	// revalidation. Refresh failures must not erase previously verified data.
	fpRefreshDue bool
	// fpRefresh invalidates only the current in-flight result when its endpoint
	// changes underneath it; the replacement probe remains due.
	fpRefresh bool
	fpAt      time.Time
	fpNext    time.Time
	pinned    bool
}

// FingerprintRetry describes the next retry after a completed probe failure.
// Attempts is consecutive for the current record and resets after success or an
// endpoint change.
type FingerprintRetry struct {
	Attempts     int
	RetryAt      time.Time
	BecameFailed bool
	Refresh      bool
}

// Observation distinguishes a valid sighting from a record that was actually
// applied. A stale signed ENR still updates liveness/protocol counters, but its
// endpoint must never drive geo enrichment or a fingerprint probe.
type Observation struct {
	Accepted bool
	Applied  bool
	Changed  bool
	New      bool
	// Reject: "no_address" | "address_policy" | "mixed_address" | "capacity" (bounded metric label values).
	Reject string
	// Evicted lower-priority nodes that made room for this one, by class name.
	Evicted      int
	EvictedClass string
}

// Capacity classes, highest priority first.
var capacityClassNames = [...]string{"verified", "classified", "unclassified", "fallback"}

func (n *Node) capacityClass() int {
	switch {
	case n.fpDone || n.pinned:
		return 0
	case n.Score <= dropBelow:
		return 3
	case n.Network != "":
		return 1
	default:
		return 2
	}
}

// evictForLocked frees up to one batch of slots from the lowest-priority
// eligible class, with deterministic oldest/lowest-score/ID ordering.
func (s *Set) evictForLocked(candClass int) (int, string) {
	batch := max(64, s.max/1000)
	worst := candClass
	var victims []*Node
	for _, n := range s.m {
		c := n.capacityClass()
		// Equal-priority nodes are never victims. Once a lower class becomes the
		// worst class, retain its same-class peers so eviction can fill a batch.
		if c <= candClass || c < worst || n.pinned {
			continue
		}
		if c > worst {
			worst = c
			victims = victims[:0]
		}
		victims = append(victims, n)
	}
	if len(victims) == 0 {
		return 0, ""
	}
	sort.Slice(victims, func(i, j int) bool {
		if victims[i].Score != victims[j].Score {
			return victims[i].Score < victims[j].Score
		}
		if !victims[i].LastResolved.Equal(victims[j].LastResolved) {
			return victims[i].LastResolved.Before(victims[j].LastResolved)
		}
		if !victims[i].LastSeen.Equal(victims[j].LastSeen) {
			return victims[i].LastSeen.Before(victims[j].LastSeen)
		}
		return bytes.Compare(victims[i].ID[:], victims[j].ID[:]) < 0
	})
	if len(victims) > batch {
		victims = victims[:batch]
	}
	for _, v := range victims {
		delete(s.m, v.ID)
	}
	return len(victims), capacityClassNames[worst]
}

func (s *Set) ClassCounts() [4]int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var counts [4]int
	for _, n := range s.m {
		counts[n.capacityClass()]++
	}
	return counts
}

func ClassName(i int) string { return capacityClassNames[i] }

type Set struct {
	mu  sync.RWMutex
	m   map[enode.ID]*Node
	max int
}

func NewWithLimit(max int) *Set {
	return &Set{m: make(map[enode.ID]*Node), max: max}
}

func (s *Set) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.m)
}

func (s *Set) Observe(n *enode.Node, via string, now time.Time) bool {
	return s.ObserveResult(n, via, now).Accepted
}

func (s *Set) ObserveResult(n *enode.Node, via string, now time.Time) Observation {
	return s.observe(n, via, now, "", "", false, false)
}

// ObserveFallbackResult records a signed DHT-cached record after direct ENR resolution
// failed. New records remain useful discovery leads, but repeated failures decay an
// existing node instead of rewarding it as a successful resolution.
func (s *Set) ObserveFallbackResult(n *enode.Node, via string, now time.Time) Observation {
	return s.observe(n, via, now, "", "", true, false)
}

// ObserveSeedResult records a devnet seed directly (forcing network, and treating an enode
// with an RLPx port as EL), so nodes with an unresolvable or NAT-advertised ENR are
// still fingerprinted from the seed's own endpoint.
func (s *Set) ObserveSeedResult(n *enode.Node, network string, now time.Time) Observation {
	return s.observe(n, "seed", now, network, "", false, true)
}

// ObserveDevnetResult records a discovery result in an explicitly isolated devnet.
// A TCP-capable record with no fork ID is treated as EL, matching the private
// on-demand probe override, but unlike a seed the record is not pinned.
func (s *Set) ObserveDevnetResult(n *enode.Node, via string, now time.Time) Observation {
	return s.observe(n, via, now, "devnet", "", false, false)
}

func (s *Set) ObserveDevnetFallbackResult(n *enode.Node, via string, now time.Time) Observation {
	return s.observe(n, via, now, "devnet", "", true, false)
}

// ObserveAuthenticatedEL records an unsigned endpoint only after an RLPx eth
// Status exchange authenticated identity ownership and supplied a live membership claim. It
// deliberately stores no ENR: an enode URL is not a signed node record.
func (s *Set) ObserveAuthenticatedEL(n *enode.Node, via, network, forkHash string, forkNext uint64, now time.Time) Observation {
	observed := s.observe(n, via, now, network, "el", false, false)
	if !observed.Applied {
		return observed
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if cur := s.m[n.ID()]; cur != nil {
		cur.MembershipSource = "status"
		cur.MembershipVerifiedAt = now
		cur.LastResolved = now
		cur.ForkHash = forkHash
		cur.ForkNext = forkNext
		cur.ForkSource = "status"
		cur.ForkObservedAt = now
		if cur.Seq == 0 {
			cur.ENR = ""
		}
	}
	return observed
}

// ObserveAuthenticatedCL records an unsigned TCP endpoint learned from an
// authenticated libp2p identity only after consensus Status supplied its network claim.
// It deliberately stores no ENR because libp2p identify is not an ENR signature.
func (s *Set) ObserveAuthenticatedCL(n *enode.Node, network, forkHash string, now time.Time) Observation {
	observed := s.observe(n, "inbound", now, network, "cl", false, false)
	if !observed.Applied {
		return observed
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if cur := s.m[n.ID()]; cur != nil {
		cur.MembershipSource = "status"
		cur.MembershipVerifiedAt = now
		cur.LastResolved = now
		cur.ForkHash = forkHash
		cur.ForkNext = 0
		cur.ForkSource = "status"
		cur.ForkObservedAt = now
		if cur.Seq == 0 {
			cur.ENR = ""
		}
	}
	return observed
}

func (s *Set) observe(n *enode.Node, via string, now time.Time, forceNetwork, forceLayer string, failedResolution, pin bool) Observation {
	if reject := addressReject(n); reject != "" {
		return Observation{Reject: reject}
	}
	// Avoid RLP/base64 serialization, public-key decompression, and fork
	// classification for a record already known to be stale. Recheck while
	// holding the write lock so a concurrent newer observation remains safe.
	s.mu.Lock()
	if cur := s.m[n.ID()]; cur != nil && n.Seq() < cur.Seq {
		if touchObservationLocked(cur, via, now, true, failedResolution, pin) {
			delete(s.m, n.ID())
		}
		s.mu.Unlock()
		return Observation{Accepted: true}
	}
	s.mu.Unlock()
	e := extract(n)
	if e.ip == "" && e.ip6 == "" {
		return Observation{Reject: "no_address"}
	}
	if forceNetwork != "" {
		e.network = forceNetwork
		if forceLayer != "" {
			e.layer = forceLayer
		} else if e.layer == "unknown" && (e.tcp != 0 || e.tcp6 != 0) {
			e.layer = "el"
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	id := n.ID()
	cur, ok := s.m[id]
	var evicted int
	var evictedClass string
	if !ok {
		if s.max > 0 && len(s.m) >= s.max {
			candClass := 2
			switch {
			case pin:
				candClass = 0
			case failedResolution:
				candClass = 3
			case e.network != "":
				candClass = 1
			}
			if evicted, evictedClass = s.evictForLocked(candClass); evicted == 0 {
				return Observation{Reject: "capacity"}
			}
		}
		initialScore := scoreInit
		if failedResolution {
			// A DHT-cached record becomes publishable only after one successful
			// direct resolution. This keeps unresolved one-shot leads out of DNS.
			initialScore = dropBelow
		}
		cur = &Node{ID: id, FirstSeen: now, Score: initialScore}
		s.m[id] = cur
	}
	if touchObservationLocked(cur, via, now, ok, failedResolution, pin) {
		delete(s.m, id)
		return Observation{Accepted: true, Evicted: evicted, EvictedClass: evictedClass}
	}
	// Signed ENR sequence numbers are monotonic. A stale DHT-cached record is
	// still a valid observation, but it must not roll endpoint or metadata back.
	// Seeds are re-observed periodically, so they obey the same monotonicity
	// rule after their first insertion. Otherwise an old configured seed can
	// roll back a newer record learned from the DHT.
	if ok && e.seq < cur.Seq {
		return Observation{Accepted: true, Evicted: evicted, EvictedClass: evictedClass}
	}
	changed := !ok || e.seq != cur.Seq || e.enr != cur.ENR || e.enode != cur.Enode
	if ok {
		recordChanged, expired := fingerprintRefreshReasons(cur, e, now)
		if recordChanged || expired {
			// A verified identity is stale-while-revalidate: preserve it until a
			// replacement probe succeeds. Self-declared ENR metadata has not earned
			// that trust and can still be cleared when its record changes.
			if cur.fpDone {
				cur.fpRefreshDue = true
			} else {
				cur.Client, cur.ClientVersion, cur.OS, cur.Lang, cur.Capabilities = "", "", "", "", ""
				cur.fpAt = time.Time{}
			}
			if recordChanged && cur.fpInFlight {
				cur.fpRefresh = true
			} else if !cur.fpInFlight && (recordChanged || expired) {
				cur.fpAttempts = 0
				cur.fpNext = time.Time{}
			}
		}
	}
	cur.Enode, cur.ENR, cur.Seq = e.enode, e.enr, e.seq
	cur.IP, cur.IP6, cur.TCP, cur.UDP = e.ip, e.ip6, e.tcp, e.udp
	cur.TCP6, cur.UDP6, cur.QUIC, cur.QUIC6 = e.tcp6, e.udp6, e.quic, e.quic6
	// A later, less-informative observation must not downgrade an established classification.
	if e.forkHash != "" && (cur.ForkHash != e.forkHash || cur.ForkNext != e.forkNext || cur.ForkSource == "") {
		cur.ForkHash, cur.ForkNext = e.forkHash, e.forkNext
		cur.ForkSource = "enr"
		cur.ForkObservedAt = now
	}
	if e.network != "" {
		if cur.Network != e.network {
			cur.MembershipSource = "enr"
			cur.MembershipVerifiedAt = time.Time{}
		}
		cur.Network = e.network
		if cur.MembershipSource == "" {
			cur.MembershipSource = "enr"
		}
	}
	if e.layer != "unknown" {
		cur.Layer = e.layer
	}
	if cur.Client == "" && e.client != "" {
		cur.Client, cur.ClientVersion = e.client, e.version
	}
	return Observation{Accepted: true, Applied: true, Changed: changed, New: !ok, Evicted: evicted, EvictedClass: evictedClass}
}

func (s *Set) CountUnclassified() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := 0
	for _, node := range s.m {
		if node.Network == "" {
			n++
		}
	}
	return n
}

// touchObservationLocked updates score and liveness and reports whether the
// node should be deleted. s.mu must be held.
func touchObservationLocked(cur *Node, via string, now time.Time, existed, failedResolution, pin bool) bool {
	if existed && failedResolution && !cur.pinned {
		if penalizeLocked(cur) {
			return true
		}
	} else if existed && !failedResolution && cur.Score < scoreCap {
		cur.Score++
	}
	if pin {
		cur.Score = scoreCap
		cur.pinned = true
	}
	cur.LastSeen = now
	cur.LastCheck = now
	if !failedResolution && (via == "v4" || via == "v5") {
		cur.LastResolved = now
	}
	switch via {
	case "v4":
		cur.HasV4 = true
	case "v5":
		cur.HasV5 = true
	}
	return false
}

func penalizeLocked(cur *Node) bool {
	cur.Score = cur.Score/2 - penaltyStep
	if cur.Score > dropBelow {
		return false
	}
	// Preserve retry state after a peer has already cost a fingerprint
	// connection; normal idle pruning will eventually remove it.
	if !cur.fpDone && cur.fpAttempts == 0 && !cur.fpInFlight {
		return true
	}
	cur.Score = dropBelow
	return false
}

func fingerprintRefreshReasons(cur *Node, next extracted, now time.Time) (recordChanged, expired bool) {
	endpointChanged := next.ip != cur.IP || next.ip6 != cur.IP6 || next.tcp != cur.TCP || next.tcp6 != cur.TCP6 ||
		next.quic != cur.QUIC || next.quic6 != cur.QUIC6
	// ENR client versions are not consistently formatted like identify/RLPx
	// versions, so only a client-name change is an immediate refresh signal.
	clientChanged := cur.Client != "" && next.client != "" && next.client != cur.Client
	expired = cur.fpDone && !cur.fpRefreshDue && (cur.fpAt.IsZero() || now.Sub(cur.fpAt) >= fpRefreshAge)
	return endpointChanged || clientChanged, expired
}

func (s *Set) ClaimFingerprint(id enode.ID) bool {
	return s.ClaimFingerprintAt(id, time.Now())
}

// ClaimFingerprintAt atomically reserves a due probe. The due-time check keeps
// repeated discovery observations from defeating fingerprint backoff.
func (s *Set) ClaimFingerprintAt(id enode.ID, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.m[id]
	if !ok || !n.fingerprintDue(now) {
		return false
	}
	n.fpAttempts++
	n.fpInFlight = true
	return true
}

type fingerprintCandidate struct {
	enr, enode string
	due        time.Time
	score      int
	id         enode.ID
}

func fingerprintCandidateBetter(a, b fingerprintCandidate) bool {
	if !a.due.Equal(b.due) {
		return a.due.Before(b.due)
	}
	if a.score != b.score {
		return a.score > b.score
	}
	return bytes.Compare(a.id[:], b.id[:]) < 0
}

// fingerprintCandidateHeap keeps the worst retained candidate at the root, so
// a scan over the full set needs only O(limit) memory and O(nodes log limit) work.
type fingerprintCandidateHeap []fingerprintCandidate

func (h fingerprintCandidateHeap) Len() int { return len(h) }
func (h fingerprintCandidateHeap) Less(i, j int) bool {
	return fingerprintCandidateBetter(h[j], h[i])
}
func (h fingerprintCandidateHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *fingerprintCandidateHeap) Push(value any) {
	*h = append(*h, value.(fingerprintCandidate))
}
func (h *fingerprintCandidateHeap) Pop() any {
	old := *h
	last := old[len(old)-1]
	*h = old[:len(old)-1]
	return last
}

// FingerprintCandidates returns retained records whose retry delay has elapsed.
// Candidates are ordered by earliest effective due time, then highest score, then
// ID so a hard limit cannot starve a fixed map-iteration subset under load.
// ClaimFingerprintAt must still be called before enqueueing: another discovery
// worker may reserve the same node after this read-only scan.
func (s *Set) FingerprintCandidates(now time.Time, limit int) []*enode.Node {
	if limit <= 0 {
		return nil
	}
	s.mu.RLock()
	selected := make(fingerprintCandidateHeap, 0, min(limit, len(s.m)))
	for _, n := range s.m {
		if !n.fingerprintDue(now) {
			continue
		}
		due := n.fpNext
		if due.IsZero() {
			due = n.FirstSeen
		}
		candidate := fingerprintCandidate{enr: n.ENR, enode: n.Enode, due: due, score: n.Score, id: n.ID}
		if len(selected) < limit {
			heap.Push(&selected, candidate)
		} else if fingerprintCandidateBetter(candidate, selected[0]) {
			selected[0] = candidate
			heap.Fix(&selected, 0)
		}
	}
	s.mu.RUnlock()

	raw := []fingerprintCandidate(selected)
	sort.Slice(raw, func(i, j int) bool { return fingerprintCandidateBetter(raw[i], raw[j]) })

	out := make([]*enode.Node, 0, len(raw))
	for _, c := range raw {
		if n := nodeRecordToEnode(c.enr, c.enode); n != nil {
			out = append(out, n)
		}
	}
	return out
}

// nodeRecordToEnode rebuilds a node from stored records, falling back to the enode URL for V4-only nodes whose synthesized ENR fails ValidSchemes.
func nodeRecordToEnode(enrStr, enodeURL string) *enode.Node {
	if enrStr != "" {
		if n, err := enode.Parse(enode.ValidSchemes, enrStr); err == nil && n != nil {
			return n
		}
	}
	if enodeURL != "" {
		if n, err := enode.ParseV4(enodeURL); err == nil && n != nil {
			return n
		}
	}
	return nil
}

// CurrentNode reconstructs the canonical record the set holds for id (may be newer than a caller's stale copy); nil if the id is no longer tracked.
func (s *Set) CurrentNode(id enode.ID) *enode.Node {
	s.mu.RLock()
	n, ok := s.m[id]
	var enrStr, enodeURL string
	if ok {
		enrStr, enodeURL = n.ENR, n.Enode
	}
	s.mu.RUnlock()
	if !ok {
		return nil
	}
	return nodeRecordToEnode(enrStr, enodeURL)
}

// geoAddrIs reports whether ip is the address enrichment would run against now. It must be the
// single preferred address rather than any advertised one: enode resolves one endpoint per record
// (IPv4 wins the locality tie among globally-routable addresses), so accepting the other family
// lets a dual-stack node keep a tunnel-brokered IPv6 location after IPv4 became preferred.
func (n *Node) geoAddrIs(ip net.IP) bool {
	if ip == nil {
		return false
	}
	preferred := n.IP
	if preferred == "" {
		preferred = n.IP6
	}
	return preferred != "" && net.ParseIP(preferred).Equal(ip)
}

// fingerprintDue is the single definition of "this node wants a probe now". ClaimFingerprintAt and
// FingerprintCandidates must agree exactly: the scan selects candidates and the claim re-checks them
// under the write lock, so any drift between the two silently starves or double-probes nodes.
func (n *Node) fingerprintDue(now time.Time) bool {
	switch {
	case n.fpDone && !n.fpRefreshDue:
		return false
	case n.fpInFlight:
		return false
	case !n.fingerprintable():
		return false
	default:
		return n.fpNext.IsZero() || !now.Before(n.fpNext)
	}
}

func (n *Node) fingerprintable() bool {
	switch n.Layer {
	case "el":
		return (n.IP != "" && n.TCP != 0) || (n.IP6 != "" && (n.TCP6 != 0 || n.TCP != 0))
	case "cl":
		return (n.IP != "" && (n.TCP != 0 || n.QUIC != 0)) ||
			(n.IP6 != "" && (n.TCP6 != 0 || n.TCP != 0 || n.QUIC6 != 0 || n.QUIC != 0))
	default:
		return false
	}
}

// fpStatus values: "ok" (freshly verified), "stale" (last success retained while
// background revalidation is due), "failed" (no successful fingerprint and
// several consecutive probes failed), "pending" (initial/retry probes in
// progress), and "n/a" (no dialable port to probe).
func (n *Node) fpStatus() string {
	switch {
	case n.fpDone && n.fpRefreshDue:
		return "stale"
	case n.fpDone:
		return "ok"
	case !n.fingerprintable():
		return "n/a"
	case n.fpAttempts >= fpFailedAfter:
		return "failed"
	default:
		return "pending"
	}
}

func (s *Set) LayerOf(id enode.ID) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if n := s.m[id]; n != nil {
		return n.Layer
	}
	return ""
}

func (s *Set) NetworkOf(id enode.ID) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if n := s.m[id]; n != nil {
		return n.Network
	}
	return ""
}

func (s *Set) IPOf(id enode.ID) net.IP {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if n := s.m[id]; n != nil {
		if n.IP != "" {
			return net.ParseIP(n.IP)
		}
		return net.ParseIP(n.IP6)
	}
	return nil
}

func (s *Set) SetExecutionStatus(id enode.ID, network string, fork forkid.ID) bool {
	return s.SetExecutionStatusAt(id, network, fork, time.Now())
}

func (s *Set) SetExecutionStatusAt(id enode.ID, network string, fork forkid.ID, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.m[id]
	if n == nil || n.Layer != "el" || network == "" {
		return false
	}
	// Authenticated live Status is authoritative over discovery metadata, which
	// may outlive an operator repurposing the same node key on another network.
	n.Network = network
	n.ForkHash = hex.EncodeToString(fork.Hash[:])
	n.ForkNext = fork.Next
	n.MembershipSource = "status"
	n.MembershipVerifiedAt = now
	n.ForkSource = "status"
	n.ForkObservedAt = now
	n.LastResolved = now
	return true
}

// SetConsensusStatus mirrors SetExecutionStatus for consensus Status results.
func (s *Set) SetConsensusStatus(id enode.ID, network, forkHash string) bool {
	return s.SetConsensusStatusAt(id, network, forkHash, time.Now())
}

func (s *Set) SetConsensusStatusAt(id enode.ID, network, forkHash string, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.m[id]
	if n == nil || n.Layer != "cl" || network == "" {
		return false
	}
	n.Network = network
	if forkHash != "" {
		n.ForkHash = forkHash
	}
	n.MembershipSource = "status"
	n.MembershipVerifiedAt = now
	n.ForkSource = "status"
	n.ForkObservedAt = now
	n.LastResolved = now
	return true
}

func (s *Set) UnclaimFingerprint(id enode.ID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n := s.m[id]; n != nil && n.fpInFlight {
		if n.fpAttempts > 0 {
			n.fpAttempts--
		}
		n.fpInFlight = false
		if n.fpRefresh {
			n.fpAttempts = 0
			n.fpRefresh = false
		}
	}
}

// FingerprintFailed schedules a bounded, jittered retry. It never permanently
// gives up on a retained node, but reaches multi-hour intervals after repeated
// rejection so the crawler does not harm peers or its own reputation.
func (s *Set) FingerprintFailed(id enode.ID, now time.Time) FingerprintRetry {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n := s.m[id]; n != nil {
		refresh := n.fpDone && n.fpRefreshDue
		n.fpInFlight = false
		// An on-demand or inbound probe may have succeeded while this attempt was
		// outstanding. A late failure must not schedule over that success.
		if n.fpDone && !n.fpRefreshDue {
			n.fpRefresh = false
			return FingerprintRetry{}
		}
		if n.fpRefresh {
			n.fpAttempts = 0
			n.fpRefresh = false
			n.fpNext = time.Time{}
			return FingerprintRetry{Refresh: refresh}
		}
		n.fpNext = now.Add(fingerprintRetryDelay(id, n.fpAttempts))
		return FingerprintRetry{Attempts: n.fpAttempts, RetryAt: n.fpNext, BecameFailed: n.fpAttempts == fpFailedAfter, Refresh: refresh}
	}
	return FingerprintRetry{}
}

func fingerprintRetryDelay(id enode.ID, attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	i := attempt - 1
	if i >= len(fpRetrySchedule) {
		i = len(fpRetrySchedule) - 1
	}
	base := fpRetrySchedule[i]
	// Stable per-node/per-attempt jitter in [90%, 110%] prevents a crawler
	// restart from producing synchronized retry waves across many peers.
	b := id[(attempt-1)%len(id)]
	permille := int64(900 + int(b)*200/255)
	return time.Duration(int64(base) * permille / 1000)
}

// SetFingerprint records a successful unclaimed probe (inbound or on-demand)
// and returns the number of failures that preceded it for this record. Callers
// use that to report recoveries without exposing node IDs as metric labels.
func (s *Set) SetFingerprint(id enode.ID, client, version, os, lang, caps, direction string) int {
	return s.SetFingerprintAt(id, client, version, os, lang, caps, direction, time.Now())
}

func (s *Set) SetFingerprintAt(id enode.ID, client, version, os, lang, caps, direction string, now time.Time) int {
	failures, _ := s.setFingerprint(id, client, version, os, lang, caps, direction, false, now)
	return failures
}

// SetClaimedFingerprint completes the probe reserved by ClaimFingerprint; it is discarded (applied
// is false) when the record changed after the claim, so the caller must not apply any other result
// of the same probe either.
func (s *Set) SetClaimedFingerprint(id enode.ID, client, version, os, lang, caps, direction string) (failures int, applied bool) {
	return s.SetClaimedFingerprintAt(id, client, version, os, lang, caps, direction, time.Now())
}

func (s *Set) SetClaimedFingerprintAt(id enode.ID, client, version, os, lang, caps, direction string, now time.Time) (failures int, applied bool) {
	return s.setFingerprint(id, client, version, os, lang, caps, direction, true, now)
}

func (s *Set) setFingerprint(id enode.ID, client, version, os, lang, caps, direction string, claimed bool, now time.Time) (int, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.m[id]
	if n == nil {
		return 0, false
	}
	if n.fpRefresh && claimed {
		n.fpAttempts = 0
		n.fpInFlight = false
		n.fpRefresh = false
		n.fpNext = time.Time{}
		return 0, false
	}
	failures := n.fpAttempts
	if n.fpInFlight && failures > 0 {
		failures--
	}
	n.Client, n.ClientVersion, n.OS, n.Lang, n.Capabilities = client, version, os, lang, caps
	n.FPDirection = direction
	// fpRefresh stays armed so the outstanding claimed probe's stale completion is discarded.
	if !n.fpRefresh {
		n.fpInFlight = false
	}
	n.fpDone = true
	n.fpRefreshDue = false
	n.fpAt = now
	n.LastResolved = now
	n.fpAttempts = 0
	n.fpNext = time.Time{}
	return failures, true
}

// SetGeo keeps prior values for a field group its lookup produced nothing for - a missing GeoIP database or unmapped IP must not wipe known data.
// lookedUp is the address the enrichment ran against. Callers resolve geo outside the lock, so a
// concurrent observation can move the record to another address first; the result is discarded
// rather than attributing the old location to the new endpoint. Sequence is not usable as the
// guard because unsigned inbound records are always sequence zero.
func (s *Set) SetGeo(id enode.ID, lookedUp net.IP, country, city, subdivision string, lat, lon float64, asn uint, org string, hosting, hostingKnown, geolocated bool, accuracyRadiusKM uint16) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.m[id]
	if n == nil || !n.geoAddrIs(lookedUp) {
		return
	}
	if geolocated {
		n.Country, n.City, n.Subdivision, n.Lat, n.Lon = country, city, subdivision, lat, lon
		n.Geolocated, n.GeoAccuracyRadiusKM = true, accuracyRadiusKM
	}
	if asn != 0 || org != "" {
		n.ASN, n.Org, n.Hosting, n.HostingKnown = asn, org, hosting, hostingKnown
	}
}

func (s *Set) Penalize(id enode.ID, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.m[id]
	if !ok {
		return
	}
	cur.LastCheck = now
	if cur.pinned {
		return
	}
	if penalizeLocked(cur) {
		delete(s.m, id)
	}
}

// PruneStaleWithVerified gives nodes with a retained successful fingerprint a
// longer real-world lifetime than unverified discovery leads. A zero verified
// cutoff disables age pruning for verified nodes.
func (s *Set) PruneStaleWithVerified(cutoff, verifiedCutoff time.Time) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	removed := 0
	for id, n := range s.m {
		stale := n.LastSeen.Before(cutoff)
		if n.fpDone {
			stale = !verifiedCutoff.IsZero() && (n.LastResolved.IsZero() || n.LastResolved.Before(verifiedCutoff))
		}
		if !n.pinned && stale {
			delete(s.m, id)
			removed++
		}
	}
	return removed
}

type extracted struct {
	enode, enr        string
	seq               uint64
	ip, ip6           string
	tcp, udp          int
	tcp6, udp6        int
	quic, quic6       int
	network, forkHash string
	forkNext          uint64
	layer             string
	client, version   string
}

// Inspect classifies a node's ENR as Observe does but records nothing; routable
// is false when no globally-routable IP is advertised.
func Inspect(n *enode.Node) (layer, network string, routable bool) {
	if addressReject(n) != "" {
		return "unknown", "", false
	}
	e := extract(n)
	return e.layer, e.network, e.ip != "" || e.ip6 != ""
}

// InspectDevnet applies the same isolated-devnet EL override as ObserveDevnet.
func InspectDevnet(n *enode.Node) (layer, network string, routable bool) {
	layer, network, routable = Inspect(n)
	if routable && layer == "unknown" && HasExecutionTCP(n) {
		layer = "el"
	}
	return
}

func extract(n *enode.Node) extracted {
	e := extracted{enode: n.URLv4(), enr: enrText(n.Record()), seq: n.Seq()}

	var ip4 enr.IPv4
	if n.Load(&ip4) == nil {
		e.ip = net4(ip4)
	}
	var ip6 enr.IPv6
	if n.Load(&ip6) == nil {
		e.ip6 = net6(ip6)
	}
	var tcp enr.TCP
	if n.Load(&tcp) == nil {
		e.tcp = int(tcp)
	}
	var udp enr.UDP
	if n.Load(&udp) == nil {
		e.udp = int(udp)
	}
	var tcp6 enr.TCP6
	if n.Load(&tcp6) == nil {
		e.tcp6 = int(tcp6)
	}
	var udp6 enr.UDP6
	if n.Load(&udp6) == nil {
		e.udp6 = int(udp6)
	}
	var quic enr.QUIC
	if n.Load(&quic) == nil {
		e.quic = int(quic)
	}
	var quic6 enr.QUIC6
	if n.Load(&quic6) == nil {
		e.quic6 = int(quic6)
	}
	var eth netconf.EthEntry
	hasEth := n.Load(&eth) == nil
	if hasEth {
		e.forkHash = hex.EncodeToString(eth.ForkID.Hash[:])
		e.forkNext = eth.ForkID.Next
		e.network = netconf.Classify(eth.ForkID)
	}
	var att netconf.AttnetsEntry
	var eth2 netconf.Eth2Entry
	loadedEth2 := n.Load(&eth2) == nil
	loadedAtt := n.Load(&att) == nil
	switch {
	case hasEth:
		e.layer = "el"
	case loadedEth2 || loadedAtt:
		e.layer = "cl"
		e.enode = ""
		if loadedEth2 && len(eth2) >= 4 {
			var d [4]byte
			copy(d[:], eth2[:4])
			e.forkHash = hex.EncodeToString(eth2[:4])
			// SSZ ENRForkID: fork_digest [0:4], next_fork_version [4:8], next_fork_epoch [8:16].
			if len(eth2) >= 16 {
				e.forkNext = binary.LittleEndian.Uint64(eth2[8:16])
			}
			if nw := netconf.ClassifyCL(d); nw != "" {
				e.network = nw
			}
		}
	default:
		e.layer = "unknown"
	}

	var ce clientEntry
	if n.Load(&ce) == nil && len(ce) >= 1 {
		if len(ce) >= 2 {
			e.version = ce[1]
		}
		e.client = clientname.CanonicalVersion(e.layer, ce[0], e.version)
	}
	return e
}

type clientEntry []string

func (clientEntry) ENRKey() string { return "client" }

type Row struct {
	ID                   string  `parquet:"id"`
	Enode                string  `parquet:"enode"`
	ENR                  string  `parquet:"enr"`
	Seq                  uint64  `parquet:"seq"`
	IP                   string  `parquet:"ip"`
	IP6                  string  `parquet:"ip6"`
	TCP                  int32   `parquet:"tcp"`
	UDP                  int32   `parquet:"udp"`
	TCP6                 int32   `parquet:"tcp6"`
	UDP6                 int32   `parquet:"udp6"`
	QUIC                 int32   `parquet:"quic"`
	QUIC6                int32   `parquet:"quic6"`
	Network              string  `parquet:"network"`
	ForkHash             string  `parquet:"fork_hash"`
	ForkNext             uint64  `parquet:"fork_next"`
	Layer                string  `parquet:"layer"`
	HasV4                bool    `parquet:"has_v4"`
	HasV5                bool    `parquet:"has_v5"`
	Score                int32   `parquet:"score"`
	FirstSeen            int64   `parquet:"first_seen"`
	LastSeen             int64   `parquet:"last_seen"`
	LastCheck            int64   `parquet:"last_check"`
	LastResolved         int64   `parquet:"last_resolved"`
	Client               string  `parquet:"client"`
	Version              string  `parquet:"client_version"`
	OS                   string  `parquet:"os"`
	Lang                 string  `parquet:"lang"`
	Caps                 string  `parquet:"capabilities"`
	Country              string  `parquet:"country"`
	City                 string  `parquet:"city"`
	Subdivision          string  `parquet:"subdivision"`
	Lat                  float64 `parquet:"lat"`
	Lon                  float64 `parquet:"lon"`
	ASN                  uint32  `parquet:"asn"`
	Org                  string  `parquet:"org"`
	Hosting              bool    `parquet:"hosting"`
	HostingKnown         bool    `parquet:"hosting_known"`
	Geolocated           bool    `parquet:"geolocated"`
	GeoAccuracyRadiusKM  uint16  `parquet:"geo_accuracy_radius_km"`
	FPStatus             string  `parquet:"fp_status"`
	FPAttempts           int32   `parquet:"fp_attempts"`
	FPAt                 int64   `parquet:"fp_at"`
	FPNext               int64   `parquet:"fp_next"`
	MembershipSource     string  `parquet:"membership_source"`
	MembershipVerifiedAt int64   `parquet:"membership_verified_at"`
	ForkSource           string  `parquet:"fork_source"`
	ForkObservedAt       int64   `parquet:"fork_observed_at"`
	FPDirection          string  `parquet:"fp_direction"`
	Pinned               bool    `parquet:"pinned"`
}

func (s *Set) CountForNetwork(network string) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := 0
	for _, node := range s.m {
		if node.Network == network {
			n++
		}
	}
	return n
}

func (s *Set) rows(network string) []Row {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows := make([]Row, 0, len(s.m))
	for _, n := range s.m {
		if network != "" && n.Network != network {
			continue
		}
		rows = append(rows, n.row())
	}
	return rows
}

// snapshotEligible excludes DHT-served fallback records until they are resolved
// or authenticated, and excludes one-shot inbound-only identities until they
// authenticate a second time. Records remain in memory for retries.
func (n *Node) snapshotEligible() bool {
	// Never fingerprintable and invisible to every layer-filtered view, so publishing one only
	// breaks the manifest's total = EL + CL invariant. Retained for retry until a layer arrives.
	if n.Layer != "el" && n.Layer != "cl" {
		return false
	}
	// A single unsolicited transport handshake is cheap to spoof and is also
	// produced by broad Internet scanners. Unsigned records with no discovery
	// provenance need a second authenticated sighting before publication.
	if n.ENR == "" && !n.HasV4 && !n.HasV5 && !n.pinned && n.Score <= scoreInit {
		return false
	}
	return n.Score > dropBelow || n.fpDone || n.pinned
}

func (n *Node) row() Row {
	attempts := n.fpAttempts
	if n.fpInFlight && attempts > 0 {
		attempts--
	}
	return Row{
		ID: n.ID.String(), Enode: n.Enode, ENR: n.ENR, Seq: n.Seq,
		IP: n.IP, IP6: n.IP6, TCP: int32(n.TCP), UDP: int32(n.UDP),
		TCP6: int32(n.TCP6), UDP6: int32(n.UDP6), QUIC: int32(n.QUIC), QUIC6: int32(n.QUIC6),
		Network: n.Network, ForkHash: n.ForkHash, ForkNext: n.ForkNext, Layer: n.Layer,
		HasV4: n.HasV4, HasV5: n.HasV5, Score: int32(n.Score),
		FirstSeen: n.FirstSeen.Unix(), LastSeen: n.LastSeen.Unix(), LastCheck: n.LastCheck.Unix(), LastResolved: unixOrZero(n.LastResolved),
		Client: n.Client, Version: n.ClientVersion, OS: n.OS, Lang: n.Lang, Caps: n.Capabilities,
		Country: n.Country, City: n.City, Subdivision: n.Subdivision, Lat: n.Lat, Lon: n.Lon, ASN: uint32(n.ASN), Org: n.Org, Hosting: n.Hosting, HostingKnown: n.HostingKnown,
		Geolocated: n.Geolocated, GeoAccuracyRadiusKM: n.GeoAccuracyRadiusKM,
		FPStatus: n.fpStatus(), FPAttempts: int32(attempts), FPAt: unixOrZero(n.fpAt), FPNext: unixOrZero(n.fpNext),
		MembershipSource: n.MembershipSource, MembershipVerifiedAt: unixOrZero(n.MembershipVerifiedAt),
		ForkSource: n.ForkSource, ForkObservedAt: unixOrZero(n.ForkObservedAt), FPDirection: n.FPDirection, Pinned: n.pinned,
	}
}

func unixOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

func timeOrZero(v int64) time.Time {
	if v == 0 {
		return time.Time{}
	}
	return time.Unix(v, 0)
}

// SnapshotNetworks captures every listed network's rows under one read lock, so a
// generation's per-network row sets and counts are mutually consistent.
func (s *Set) SnapshotNetworks(networks []string) map[string][]Row {
	want := make(map[string]bool, len(networks))
	for _, n := range networks {
		want[n] = true
	}
	byNet := make(map[string][]Row, len(networks))
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, n := range s.m {
		if want[n.Network] && n.snapshotEligible() {
			byNet[n.Network] = append(byNet[n.Network], n.row())
		}
	}
	return byNet
}

func RowsFromParquet(data []byte) ([]Row, error) {
	if len(data) == 0 {
		return nil, nil
	}
	return parquet.Read[Row](bytes.NewReader(data), int64(len(data)))
}

func (s *Set) Ingest(rows []Row) (dropped, evicted int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range rows {
		b, err := hex.DecodeString(r.ID)
		if err != nil || len(b) != len(enode.ID{}) {
			dropped++
			continue
		}
		var id enode.ID
		copy(id[:], b)
		if !rowAddressesUsable(r) {
			dropped++
			continue
		}
		verifiedFingerprint := r.FPStatus == "ok" || r.FPStatus == "stale"
		candidate := &Node{
			ID: id, Enode: r.Enode, ENR: r.ENR, Seq: r.Seq,
			IP: r.IP, IP6: r.IP6, TCP: int(r.TCP), UDP: int(r.UDP),
			TCP6: int(r.TCP6), UDP6: int(r.UDP6), QUIC: int(r.QUIC), QUIC6: int(r.QUIC6),
			Network: r.Network, ForkHash: r.ForkHash, ForkNext: r.ForkNext, Layer: r.Layer,
			HasV4: r.HasV4, HasV5: r.HasV5, Score: int(r.Score),
			FirstSeen: time.Unix(r.FirstSeen, 0), LastSeen: time.Unix(r.LastSeen, 0), LastCheck: time.Unix(r.LastCheck, 0), LastResolved: timeOrZero(r.LastResolved),
			Client: r.Client, ClientVersion: r.Version, OS: r.OS, Lang: r.Lang, Capabilities: r.Caps,
			MembershipSource: r.MembershipSource, MembershipVerifiedAt: timeOrZero(r.MembershipVerifiedAt),
			ForkSource: r.ForkSource, ForkObservedAt: timeOrZero(r.ForkObservedAt), FPDirection: r.FPDirection,
			Country: r.Country, City: r.City, Subdivision: r.Subdivision, Lat: r.Lat, Lon: r.Lon, ASN: uint(r.ASN), Org: r.Org, Hosting: r.Hosting, HostingKnown: r.HostingKnown,
			Geolocated: r.Geolocated, GeoAccuracyRadiusKM: r.GeoAccuracyRadiusKM, pinned: r.Pinned,
			fpDone:       verifiedFingerprint,
			fpRefreshDue: verifiedFingerprint && r.FPStatus == "stale",
			fpAttempts:   restoredAttempts(r),
			fpAt:         timeOrZero(r.FPAt),
			fpNext:       timeOrZero(r.FPNext),
		}
		if _, exists := s.m[id]; !exists && s.max > 0 && len(s.m) >= s.max {
			n, _ := s.evictForLocked(candidate.capacityClass())
			if n == 0 {
				dropped++
				continue
			}
			evicted += n
		}
		s.m[id] = candidate
	}
	return dropped, evicted
}

func restoredAttempts(r Row) int {
	if r.FPAttempts < 0 {
		return 0
	}
	return int(r.FPAttempts)
}

func (s *Set) ParquetForNetwork(network string) ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows := make([]Row, 0, len(s.m))
	for _, n := range s.m {
		if (network == "" || n.Network == network) && n.snapshotEligible() {
			rows = append(rows, n.row())
		}
	}
	return ParquetFromRows(rows)
}

func ParquetFromRows(rows []Row) ([]byte, error) {
	var buf bytes.Buffer
	w := parquet.NewGenericWriter[Row](&buf)
	if len(rows) > 0 {
		if _, err := w.Write(rows); err != nil {
			w.Close()
			return nil, err
		}
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func enrText(r *enr.Record) string {
	b, err := rlp.EncodeToBytes(r)
	if err != nil {
		return ""
	}
	return "enr:" + base64.RawURLEncoding.EncodeToString(b)
}

func net4(ip enr.IPv4) string {
	return usableIP(net.IP(ip[:]))
}

func net6(ip enr.IPv6) string {
	return usableIP(net.IP(ip[:]))
}

// AllowPrivateIPs keeps otherwise-dropped private addresses; enable only for an isolated devnet.
var AllowPrivateIPs bool

func usableIP(ip net.IP) string {
	if !netpolicy.Usable(ip, AllowPrivateIPs) {
		return ""
	}
	return ip.String()
}

func addressReject(n *enode.Node) string {
	present, usable := 0, 0
	var ip4 enr.IPv4
	if n.Load(&ip4) == nil {
		present++
		if netpolicy.Usable(net.IP(ip4[:]), AllowPrivateIPs) {
			usable++
		}
	}
	var ip6 enr.IPv6
	if n.Load(&ip6) == nil {
		present++
		if netpolicy.Usable(net.IP(ip6[:]), AllowPrivateIPs) {
			usable++
		}
	}
	switch {
	case present == 0:
		return "no_address"
	case usable == present:
		return ""
	case usable > 0:
		return "mixed_address"
	default:
		return "address_policy"
	}
}

// rowAddressesUsable keeps restored rows from bypassing the observe-time address policy after a laxer run (e.g. --allow-private-ips).
func rowAddressesUsable(r Row) bool {
	hasAddress := false
	for _, s := range []string{r.IP, r.IP6} {
		if s == "" {
			continue
		}
		ip := net.ParseIP(s)
		if ip == nil || usableIP(ip) == "" {
			return false
		}
		hasAddress = true
	}
	return hasAddress
}

// Dialable reports an advertised application transport for a present address family; absent tcp6/quic6 fall back to tcp/quic per the ENR endpoint rules.
func (r Row) Dialable() bool {
	v4 := r.IP != "" && (r.TCP != 0 || r.QUIC != 0)
	return v4 || r.DialableV6()
}

// DialableV6 reports a reachable IPv6 endpoint. Per the ENR spec an absent tcp6/udp6 means tcp/udp
// applies to v6 as well, so an inherited port still counts.
func (r Row) DialableV6() bool {
	return r.IP6 != "" && (r.TCP6 != 0 || r.TCP != 0 || r.QUIC6 != 0 || r.QUIC != 0)
}

// HasExecutionTCP reports whether a record advertises an RLPx-capable endpoint.
// It is only a dialability hint: callers must authenticate network membership
// with eth Status before classifying an otherwise unclassified record as EL.
func HasExecutionTCP(n *enode.Node) bool {
	var tcp enr.TCP
	if n.Load(&tcp) == nil && tcp != 0 {
		return true
	}
	var tcp6 enr.TCP6
	return n.Load(&tcp6) == nil && tcp6 != 0
}
