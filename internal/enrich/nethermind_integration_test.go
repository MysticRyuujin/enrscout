package enrich

import (
	"context"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/p2p/enode"

	"github.com/MysticRyuujin/enrscout/internal/netconf"
)

// TestNethermindRLPxCompatibility exercises real release images when explicitly
// requested. It is intentionally opt-in because it requires Docker and downloads
// roughly 2 GB of images for the full historical matrix.
func TestNethermindRLPxCompatibility(t *testing.T) {
	rawImages := os.Getenv("ENRSCOUT_NETHERMIND_IMAGES")
	if rawImages == "" {
		t.Skip("set ENRSCOUT_NETHERMIND_IMAGES to run the Docker compatibility matrix")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is unavailable")
	}

	mainnet, err := netconf.Get("mainnet")
	if err != nil {
		t.Fatal(err)
	}

	for _, image := range strings.Fields(rawImages) {
		image := image
		t.Run(strings.ReplaceAll(image, "/", "_"), func(t *testing.T) {
			testNethermindImage(t, mainnet, image)
		})
	}
}

func testNethermindImage(t *testing.T, mainnet *netconf.Network, image string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	inbound, err := NewFingerprinterWithPolicy(15*time.Second, 4, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	results := make(chan InboundFingerprint, 1)
	ln, err := inbound.ListenInbound(ctx, "0.0.0.0:0", mainnet, func(result InboundFingerprint) {
		select {
		case results <- result:
		default:
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	listenPort := ln.Addr().(*net.TCPAddr).Port
	hostIP := privateTestIPv4(t)
	staticPeer := fmt.Sprintf("enode://%s@%s:%d", hex.EncodeToString(inbound.id), hostIP, listenPort)

	name := "enrscout-rlpx-" + sanitizeContainerName(image) + "-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	containerID := strings.TrimSpace(runDocker(t,
		"run", "--rm", "-d", "--name", name,
		"-p", "0.0.0.0::30303",
		image,
		"--config=mainnet",
		"--datadir=/tmp/nethermind",
		"--Network.P2PPort=30303",
		"--Network.DiscoveryPort=30303",
		"--Network.OnlyStaticPeers=true",
		"--Network.MaxActivePeers=100",
		"--Sync.NetworkingEnabled=true",
		"--Sync.SynchronizationEnabled=false",
	))
	if containerID == "" {
		t.Fatal("docker run returned an empty container ID")
	}
	defer stopDockerContainer(t, name)

	logs := waitForNethermind(t, name)
	pubMatch := regexp.MustCompile(`enode://([0-9a-fA-F]{128})@`).FindStringSubmatch(logs)
	if len(pubMatch) != 2 {
		t.Fatalf("could not find node public key in logs:\n%s", logs)
	}
	pubBytes, err := hex.DecodeString("04" + pubMatch[1])
	if err != nil {
		t.Fatal(err)
	}
	pub, err := crypto.UnmarshalPubkey(pubBytes)
	if err != nil {
		t.Fatal(err)
	}
	port := mappedDockerPort(t, name, "30303/tcp")
	n := enode.NewV4(pub, hostIP, port, 0)

	outbound, err := NewFingerprinterWithPolicy(15*time.Second, 1, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	fp, err := outbound.ProbeStatus(context.Background(), n, mainnet)
	if err != nil {
		t.Fatalf("outbound ProbeStatus: %v\n%s", err, currentDockerLogs(name))
	}
	assertNethermindFingerprint(t, image, "outbound", fp)
	stopDockerContainer(t, name)

	inboundName := name + "-inbound"
	runDocker(t,
		"run", "--rm", "-d", "--name", inboundName,
		image,
		"--config=mainnet",
		"--datadir=/tmp/nethermind",
		"--Network.P2PPort=30303",
		"--Network.DiscoveryPort=30303",
		"--Network.OnlyStaticPeers=true",
		"--Network.MaxActivePeers=100",
		"--Network.StaticPeers="+staticPeer,
		"--Sync.NetworkingEnabled=true",
		"--Sync.SynchronizationEnabled=true",
	)
	defer stopDockerContainer(t, inboundName)
	waitForNethermind(t, inboundName)

	select {
	case result := <-results:
		if result.Err != nil {
			t.Fatalf("inbound listener: %v\n%s", result.Err, currentDockerLogs(inboundName))
		}
		assertNethermindFingerprint(t, image, "inbound", result.Fingerprint)
	case <-time.After(30 * time.Second):
		t.Fatalf("timed out waiting for Nethermind to dial the passive listener\n%s", currentDockerLogs(inboundName))
	}
}

func stopDockerContainer(t *testing.T, name string) {
	t.Helper()
	cmd := exec.Command("docker", "stop", "--timeout", "5", name)
	if out, err := cmd.CombinedOutput(); err != nil && !strings.Contains(string(out), "No such container") {
		t.Logf("stop %s: %v: %s", name, err, out)
	}
}

func mappedDockerPort(t *testing.T, name, containerPort string) int {
	t.Helper()
	mapped := strings.Split(strings.TrimSpace(runDocker(t, "port", name, containerPort)), "\n")[0]
	_, rawPort, err := net.SplitHostPort(mapped)
	if err != nil {
		t.Fatalf("parse mapped %s port %q: %v", containerPort, mapped, err)
	}
	port, err := strconv.Atoi(rawPort)
	if err != nil {
		t.Fatal(err)
	}
	return port
}

func assertNethermindFingerprint(t *testing.T, image, direction string, fp Fingerprint) {
	t.Helper()
	version := image[strings.LastIndex(image, ":")+1:]
	if fp.Client != "Nethermind" || !strings.HasPrefix(fp.Version, "v"+version) || fp.Network != "mainnet" {
		t.Fatalf("%s fingerprint for %s = %+v", direction, image, fp)
	}
	t.Logf("%s %s: version=%s caps=%s fork=%x/%d", image, direction, fp.Version, fp.Caps, fp.ForkID.Hash, fp.ForkID.Next)
}

func waitForNethermind(t *testing.T, name string) string {
	t.Helper()
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		logs := currentDockerLogs(name)
		if strings.Contains(logs, "Initialization Completed") && strings.Contains(logs, "enode://") {
			return logs
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("Nethermind did not become ready:\n%s", currentDockerLogs(name))
	return ""
}

func currentDockerLogs(name string) string {
	out, _ := exec.Command("docker", "logs", name).CombinedOutput()
	return string(out)
}

func runDocker(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func sanitizeContainerName(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			return r
		}
		return '-'
	}, s)
}
