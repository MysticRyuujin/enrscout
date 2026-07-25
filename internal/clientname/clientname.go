package clientname

import (
	"regexp"
	"strings"
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

// Crawlers, tooling, L2 clients (OP-Geth), and garbage self-reported strings are
// deliberately absent so aggregation collapses them to Other. Keep in sync with the
// web CLIENT_COLOR map in web/src/theme.ts.
var recognized = map[string]bool{
	"Geth": true, "Nethermind": true, "Besu": true, "Erigon": true, "Reth": true,
	"Ethrex": true, "EthereumJS": true, "Nimbus": true,
	"Lighthouse": true, "Prysm": true, "Teku": true, "Lodestar": true,
	"Grandine": true, "Caplin": true,
}

func Recognized(name string) bool {
	return recognized[strings.TrimSpace(name)]
}
