package clientname

import (
	"regexp"
	"strings"
	"time"
)

var opGethVersion = regexp.MustCompile(`(?i)^v?1\.10[0-9]{4}\.[0-9]+(?:$|[-+])`)

// Canonical normalizes client names from both self-declared ENR metadata and
// active fingerprints so aggregation never splits on casing or known aliases.
func Canonical(layer, name string) string {
	return CanonicalVersion(layer, name, "")
}

// CanonicalVersion normalizes a client name and uses distinctive version
// formats to separate forks which retain their upstream wire identity.
func CanonicalVersion(layer, name, version string) string {
	switch layer {
	case "el":
		return ExecutionVersion(name, version)
	case "cl":
		return Consensus(name)
	default:
		return strings.TrimSpace(name)
	}
}

// ExecutionVersion distinguishes OP-Geth from upstream Geth. OP-Geth keeps
// "Geth" in its RLPx Hello but uses releases such as v1.101408.0, where the
// six-digit middle component encodes the upstream Geth version.
func ExecutionVersion(name, version string) string {
	name = strings.TrimSpace(name)
	switch strings.ToLower(name) {
	case "geth", "go-ethereum":
		if opGethVersion.MatchString(strings.TrimSpace(version)) {
			return "OP-Geth"
		}
		return "Geth"
	case "op-geth", "opgeth":
		return "OP-Geth"
	case "nethermind":
		return "Nethermind"
	case "besu":
		return "Besu"
	case "erigon":
		return "Erigon"
	case "reth":
		return "Reth"
	case "ethrex":
		return "Ethrex"
	case "ethereumjs":
		return "EthereumJS"
	case "nimbus", "nimbus-eth1", "nimbusexecutionclient":
		return "Nimbus"
	default:
		return name
	}
}

func Consensus(name string) string {
	name = strings.TrimSpace(name)
	switch strings.ToLower(name) {
	case "lighthouse":
		return "Lighthouse"
	case "prysm":
		return "Prysm"
	case "nimbus":
		return "Nimbus"
	case "lodestar":
		return "Lodestar"
	case "grandine":
		return "Grandine"
	case "caplin", "erigon":
		return "Caplin"
	case "teku":
		return "Teku"
	case "nethermind":
		return "Nethermind"
	case "ethlambda":
		return "Ethlambda"
	default:
		return name
	}
}

// ConsensusAgentHasNestedVersion identifies clients whose libp2p agent string
// uses client/client/version instead of client/version.
func ConsensusAgentHasNestedVersion(client, second string) bool {
	client = strings.ToLower(strings.TrimSpace(client))
	second = strings.ToLower(strings.TrimSpace(second))
	return ((client == "erigon" || client == "caplin") && second == "caplin") ||
		(client == "teku" && second == "teku")
}

const Other = "Other"

// Self is the client name our own advertisers announce in the RLPx Hello and the libp2p agent
// string. Other ENRScout deployments therefore appear as identities, and readiness views drop them.
const Self = "enrscout"

// Crawlers, tooling, L2 clients (OP-Geth), and garbage self-reported strings are
// deliberately absent so aggregation collapses them to Other. Per layer: a name
// recognized on one layer is not evidence of a client on the other. Keep in sync with web/src/theme.ts.
var recognized = map[string]map[string]bool{
	"el": {
		"Geth": true, "Nethermind": true, "Besu": true, "Erigon": true, "Reth": true,
		"Ethrex": true, "EthereumJS": true, "Nimbus": true,
	},
	"cl": {
		"Lighthouse": true, "Prysm": true, "Teku": true, "Nimbus": true, "Lodestar": true,
		"Grandine": true, "Caplin": true, "Nethermind": true, "Ethlambda": true,
	},
}

func Recognized(layer, name string) bool {
	return recognized[layer][strings.TrimSpace(name)]
}

// ChartMaxFingerprintAge bounds how old a verified fingerprint may be and still count toward client
// charts; older identifications remain on node detail as last-known state.
const ChartMaxFingerprintAge = 7 * 24 * time.Hour

// Charted is the Go form of the query package's chart fingerprint condition, for the crawler,
// which cannot run SQL over its rows.
func Charted(fpStatus string, fpAt int64, at time.Time) bool {
	return (fpStatus == "ok" || fpStatus == "stale") && fpAt >= at.Add(-ChartMaxFingerprintAge).Unix()
}
