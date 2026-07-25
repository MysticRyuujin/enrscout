package main

import "testing"

func TestIdentitySpecs(t *testing.T) {
	specs, err := identitySpecs([]string{"mainnet", "hoodi", "sepolia"}, 30303, 1, 1, true)
	if err != nil {
		t.Fatal(err)
	}
	want := []identitySpec{
		{Network: "mainnet", Layer: layerEL, Walker: true, DiscoveryPort: 30303, TCPPort: 30303},
		{Network: "mainnet", Layer: layerCL, Walker: true, DiscoveryPort: 30304, TCPPort: 30305, QUICPort: 30305},
		{Network: "hoodi", Layer: layerEL, Walker: true, DiscoveryPort: 30306, TCPPort: 30306},
		{Network: "hoodi", Layer: layerCL, Walker: true, DiscoveryPort: 30307, TCPPort: 30308, QUICPort: 30308},
		{Network: "sepolia", Layer: layerEL, Walker: true, DiscoveryPort: 30309, TCPPort: 30309},
		{Network: "sepolia", Layer: layerCL, Walker: true, DiscoveryPort: 30310, TCPPort: 30311, QUICPort: 30311},
	}
	if len(specs) != len(want) {
		t.Fatalf("got %d specs, want %d", len(specs), len(want))
	}
	for i := range want {
		if specs[i] != want[i] {
			t.Errorf("spec[%d] = %+v, want %+v", i, specs[i], want[i])
		}
	}
}

func TestIdentitySpecsRejectOverflow(t *testing.T) {
	if _, err := identitySpecs([]string{"mainnet"}, 65534, 1, 1, true); err == nil {
		t.Fatal("overflowing port range accepted")
	}
	if _, err := identitySpecs([]string{"mainnet"}, 30303, 0, 1, true); err == nil {
		t.Fatal("zero EL identities accepted")
	}
}

func TestIdentitySpecsMultipleELIdentities(t *testing.T) {
	specs, err := identitySpecs([]string{"mainnet", "hoodi"}, 40404, 3, 1, true)
	if err != nil {
		t.Fatal(err)
	}
	want := []identitySpec{
		{Network: "mainnet", Layer: layerEL, Walker: true, DiscoveryPort: 40404},
		{Network: "mainnet", Layer: layerEL, Index: 1, DiscoveryPort: 40405},
		{Network: "mainnet", Layer: layerEL, Index: 2, DiscoveryPort: 40406},
		{Network: "mainnet", Layer: layerCL, Walker: true, DiscoveryPort: 40407},
		{Network: "hoodi", Layer: layerEL, Walker: true, DiscoveryPort: 40409},
		{Network: "hoodi", Layer: layerEL, Index: 1, DiscoveryPort: 40410},
		{Network: "hoodi", Layer: layerEL, Index: 2, DiscoveryPort: 40411},
		{Network: "hoodi", Layer: layerCL, Walker: true, DiscoveryPort: 40412},
	}
	if len(specs) != len(want) {
		t.Fatalf("got %d specs, want %d", len(specs), len(want))
	}
	for i := range want {
		want[i].TCPPort = want[i].DiscoveryPort
		if want[i].Layer == layerCL {
			want[i].TCPPort = want[i].DiscoveryPort + 1
			want[i].QUICPort = want[i].DiscoveryPort + 1
		}
		if specs[i] != want[i] {
			t.Errorf("spec[%d] = %+v, want %+v", i, specs[i], want[i])
		}
	}
	if got := identityKeyName(specs[0]); got != "mainnet-el.key" {
		t.Fatalf("first key = %q", got)
	}
	if got := identityKeyName(specs[2]); got != "mainnet-el-3.key" {
		t.Fatalf("third key = %q", got)
	}
}

func TestWalkerSelection(t *testing.T) {
	specs, err := identitySpecs([]string{"mainnet"}, 40404, 3, 0, true)
	if err != nil {
		t.Fatal(err)
	}
	for i, spec := range specs {
		if !spec.Walker {
			t.Errorf("spec[%d] %s-%s not a walker with walker-el-identities=0", i, spec.Network, spec.Layer)
		}
	}
	specs, err = identitySpecs([]string{"mainnet"}, 40404, 3, 2, true)
	if err != nil {
		t.Fatal(err)
	}
	walkers := 0
	for _, spec := range specs {
		if spec.Walker {
			walkers++
		}
	}
	if walkers != 3 {
		t.Fatalf("walkers = %d, want 2 EL + 1 CL", walkers)
	}

	el := identitySpec{Layer: layerEL, Walker: true}
	cl := identitySpec{Layer: layerCL, Walker: true}
	idle := identitySpec{Layer: layerEL}
	for _, tc := range []struct {
		spec  identitySpec
		proto string
		want  bool
	}{
		{el, "v4", true}, {el, "v5", true},
		{cl, "v4", false}, {cl, "v5", true},
		{idle, "v4", false}, {idle, "v5", false},
	} {
		if got := walkSource(tc.spec, tc.proto); got != tc.want {
			t.Errorf("walkSource(%s walker=%v, %s) = %v, want %v", tc.spec.Layer, tc.spec.Walker, tc.proto, got, tc.want)
		}
	}
}

func TestParseAdvertiserNetworks(t *testing.T) {
	got, err := parseAdvertiserNetworks("mainnet, hoodi", false)
	if err != nil || len(got) != 2 || got[1] != "hoodi" {
		t.Fatalf("parse = %v, %v", got, err)
	}
	if _, err := parseAdvertiserNetworks("mainnet,mainnet", false); err == nil {
		t.Fatal("duplicate network accepted")
	}
	if got, err := parseAdvertiserNetworks("", true); err != nil || len(got) != 1 || got[0] != "devnet" {
		t.Fatalf("devnet parse = %v, %v", got, err)
	}
}

func TestWalkerNameIsStableAndDistinct(t *testing.T) {
	if got := walkerName(identitySpec{Network: "mainnet", Layer: layerEL}); got != "mainnet-el" {
		t.Fatalf("first EL walker name = %q", got)
	}
	if got := walkerName(identitySpec{Network: "mainnet", Layer: layerEL, Index: 1}); got != "mainnet-el-2" {
		t.Fatalf("second EL walker name = %q", got)
	}
	if got := walkerName(identitySpec{Network: "mainnet", Layer: layerCL}); got != "mainnet-cl" {
		t.Fatalf("CL walker name = %q", got)
	}
}
