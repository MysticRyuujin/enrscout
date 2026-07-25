package clientname

import "testing"

func TestCanonical(t *testing.T) {
	cases := []struct {
		layer, input, want string
	}{
		{"el", "erigon", "Erigon"},
		{"el", "go-ethereum", "Geth"},
		{"el", "op-geth", "OP-Geth"},
		{"el", "nimbus-eth1", "Nimbus"},
		{"cl", "erigon", "Caplin"},
		{"cl", "TEKU", "Teku"},
		{"unknown", " custom ", "custom"},
	}
	for _, tc := range cases {
		if got := Canonical(tc.layer, tc.input); got != tc.want {
			t.Errorf("Canonical(%q, %q) = %q, want %q", tc.layer, tc.input, got, tc.want)
		}
	}
}

func TestExecutionVersionDistinguishesOPGeth(t *testing.T) {
	cases := []struct {
		name, version, want string
	}{
		{"Geth", "v1.101408.0-stable-5c2e7586", "OP-Geth"},
		{"go-ethereum", "1.101603.5", "OP-Geth"},
		{"Geth", "v1.17.4-stable", "Geth"},
		{"Geth", "v1.10140.0", "Geth"},
	}
	for _, tc := range cases {
		if got := ExecutionVersion(tc.name, tc.version); got != tc.want {
			t.Errorf("ExecutionVersion(%q, %q) = %q, want %q", tc.name, tc.version, got, tc.want)
		}
	}
}

func TestNimbusExecutionClientAlias(t *testing.T) {
	if got := ExecutionVersion("NimbusExecutionClient", ""); got != "Nimbus" {
		t.Errorf("ExecutionVersion(NimbusExecutionClient) = %q, want Nimbus", got)
	}
}

func TestRecognized(t *testing.T) {
	for _, name := range []string{"Geth", "Nethermind", "Reth", "Nimbus", "Lighthouse", "Teku", "Caplin"} {
		if !Recognized(name) {
			t.Errorf("%q should be recognized", name)
		}
	}
	for _, name := range []string{"OP-Geth", "github.com", "hermes", "rust-libp2p", "gnode", "r", "", "Other"} {
		if Recognized(name) {
			t.Errorf("%q should not be recognized", name)
		}
	}
}
