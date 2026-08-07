package enrich

import "testing"

func TestClassifyHosting(t *testing.T) {
	hosting := []struct {
		asn uint
		org string
	}{
		{16509, "Amazon.com, Inc."},
		{24940, "Hetzner Online GmbH"},
		{20326, "TeraSwitch Networks Inc."},
		{395201, "Allnodes Inc"},
		{46475, "Limestone Networks, Inc."},
		{29802, "HIVELOCITY, Inc."},
		{29066, "velia.net Internetdienste GmbH"},
		{212980, "UAB Atlantis Capital"},
		{60602, "Inovare-Prim SRL"},
		{329184, "Host Africa (Pty) Ltd"},
		{207728, "EUROHOSTER Ltd."},
		{63023, "GTHost"},
		{213095, "Exaion SAS"},
		{204770, "UAB Cherry Servers"},
		// ASN-classified: the org name carries no hosting keyword.
		{7979, "Servers.com, Inc."},
		{136907, "HUAWEI CLOUDS"},
		{19318, "Interserver, Inc"},
		{202613, "Aruba S.p.A."},
		{2734, "CoreSite"},
		{22612, "Namecheap, Inc."},
	}
	for _, tc := range hosting {
		if !ClassifyHosting(tc.asn, tc.org) {
			t.Errorf("ClassifyHosting(%d, %q) = false, want true", tc.asn, tc.org)
		}
	}
	residential := []struct {
		asn uint
		org string
	}{
		{16591, "Google Fiber Inc."},
		{16591, "GOOGLE-FIBER"},
		{7922, "Comcast Cable Communications, LLC"},
		{3320, "Deutsche Telekom AG"},
		{701, "Verizon Business"},
		{13030, "Init7 (Switzerland) Ltd."},
		{0, ""},
	}
	for _, tc := range residential {
		if ClassifyHosting(tc.asn, tc.org) {
			t.Errorf("ClassifyHosting(%d, %q) = true, want false", tc.asn, tc.org)
		}
	}
}

func TestHostingClassificationKnown(t *testing.T) {
	tests := []struct {
		asn            uint
		org            string
		hosting, known bool
	}{
		{7979, "Servers.com, Inc.", true, true},
		{24940, "Hetzner Online GmbH", true, true},
		{16591, "Google Fiber Inc.", false, true},
		{7922, "Comcast Cable Communications, LLC", false, false},
		{0, "", false, false},
	}
	for _, tc := range tests {
		hosting, known := HostingClassification(tc.asn, tc.org)
		if hosting != tc.hosting || known != tc.known {
			t.Errorf("HostingClassification(%d, %q) = (%v, %v), want (%v, %v)",
				tc.asn, tc.org, hosting, known, tc.hosting, tc.known)
		}
	}
}
