package enrich

import "testing"

func TestClassifyHosting(t *testing.T) {
	hosting := []string{
		"Amazon.com, Inc.",
		"Hetzner Online GmbH",
		"TeraSwitch Networks Inc.",
		"Allnodes Inc",
		"Limestone Networks, Inc.",
		"HIVELOCITY, Inc.",
		"velia.net Internetdienste GmbH",
		"UAB Atlantis Capital",
		"Inovare-Prim SRL",
		"Host Africa (Pty) Ltd",
		"EUROHOSTER Ltd.",
		"GTHost",
		"Exaion SAS",
		"UAB Cherry Servers",
	}
	for _, org := range hosting {
		if !ClassifyHosting(org) {
			t.Errorf("ClassifyHosting(%q) = false, want true", org)
		}
	}
	residential := []string{
		"Google Fiber Inc.",
		"GOOGLE-FIBER",
		"Comcast Cable Communications, LLC",
		"Deutsche Telekom AG",
		"Verizon Business",
		"Init7 (Switzerland) Ltd.",
		"",
	}
	for _, org := range residential {
		if ClassifyHosting(org) {
			t.Errorf("ClassifyHosting(%q) = true, want false", org)
		}
	}
}
