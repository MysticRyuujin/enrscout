package netpolicy

import (
	"net"
	"testing"
)

func TestUsable(t *testing.T) {
	cases := []struct {
		ip           string
		allowPrivate bool
		want         bool
	}{
		{"8.8.8.8", false, true},
		{"2001:4860:4860::8888", false, true},
		{"10.1.2.3", false, false},
		{"10.1.2.3", true, true},
		{"127.0.0.1", true, false},
		{"169.254.1.1", true, false},
		{"192.0.2.1", false, false},
		{"100.64.1.2", false, false},
		{"100.127.255.255", true, false},
		{"100.128.0.1", false, true},
		{"240.0.0.1", false, false},
		{"250.1.2.3", true, false},
	}
	for _, c := range cases {
		if got := Usable(net.ParseIP(c.ip), c.allowPrivate); got != c.want {
			t.Errorf("Usable(%s, allowPrivate=%v) = %v, want %v", c.ip, c.allowPrivate, got, c.want)
		}
	}
	if Usable(nil, true) {
		t.Error("nil ip accepted")
	}
}
