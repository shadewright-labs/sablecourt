package councilproxy

import (
	"net/netip"
	"testing"
)

// Every special-purpose range of the IANA IPv4 and IPv6 special-purpose
// registries that is not globally reachable, with an address inside it and
// one just outside it.
func TestPublicAddressSpecialPurposeRanges(t *testing.T) {
	for _, tc := range []struct{ name, inside, outside string }{
		{"0.0.0.0/8 this network", "0.255.255.255", "1.0.0.0"},
		{"10.0.0.0/8 private", "10.255.255.255", "11.0.0.0"},
		{"100.64.0.0/10 shared address space", "100.127.255.255", "100.128.0.0"},
		{"100.64.0.0/10 lower edge", "100.64.0.0", "100.63.255.255"},
		{"127.0.0.0/8 loopback", "127.255.255.255", "128.0.0.1"},
		{"169.254.0.0/16 link local", "169.254.169.254", "169.255.0.1"},
		{"172.16.0.0/12 private", "172.31.255.255", "172.32.0.1"},
		{"192.0.0.0/24 IETF protocol assignments", "192.0.0.255", "192.0.1.1"},
		{"192.0.2.0/24 documentation", "192.0.2.255", "192.0.3.1"},
		{"192.88.99.0/24 deprecated 6to4 relay", "192.88.99.1", "192.88.100.1"},
		{"192.168.0.0/16 private", "192.168.255.255", "192.169.0.1"},
		{"198.18.0.0/15 benchmarking", "198.19.255.255", "198.20.0.1"},
		{"198.51.100.0/24 documentation", "198.51.100.255", "198.51.101.1"},
		{"203.0.113.0/24 documentation", "203.0.113.255", "203.0.114.1"},
		{"224.0.0.0/4 multicast", "239.255.255.255", "223.255.255.255"},
		{"240.0.0.0/4 reserved and broadcast", "255.255.255.255", "223.255.255.255"},
		{"::/128 unspecified", "::", "2000::1"},
		{"::1/128 loopback", "::1", "2000::1"},
		{"::ffff:0:0/96 mapped private", "::ffff:10.0.0.1", "::ffff:1.2.3.4"},
		{"64:ff9b::/96 NAT64", "64:ff9b::808:808", "2000::1"},
		{"100::/64 discard only", "100::1", "2000::1"},
		{"2001::/23 IETF protocol assignments", "2001:1ff:ffff::1", "2001:200::1"},
		{"2001:db8::/32 documentation", "2001:db8:ffff::1", "2001:db9::1"},
		{"2002::/16 6to4", "2002:ffff::1", "2003::1"},
		{"3fff::/20 documentation (RFC 9637)", "3fff:fff:ffff::1", "3fff:1000::1"},
		{"3fff::/20 lower edge", "3fff::", "3ffe:ffff::1"},
		{"5f00::/16 SRv6 SIDs", "5f00::1", "2000::1"},
		{"fc00::/7 unique local", "fdff::1", "2000::1"},
		{"fe80::/10 link local", "febf::1", "2000::1"},
		{"ff00::/8 multicast", "ff02::1", "2000::1"},
		{"outside 2000::/3", "4000::1", "2000::1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if publicAddress(netip.MustParseAddr(tc.inside)) {
				t.Errorf("%s is inside the range and must be refused", tc.inside)
			}
			if !publicAddress(netip.MustParseAddr(tc.outside)) {
				t.Errorf("%s is just outside the range and must be allowed", tc.outside)
			}
		})
	}
}

func TestPublicAddressRejectsZonedAddresses(t *testing.T) {
	for _, in := range []string{"::ffff:1.2.3.4%eth0", "2001:4860:4860::8888%eth0", "::ffff:10.0.0.1%eth0", "fe80::1%eth0"} {
		if publicAddress(netip.MustParseAddr(in)) {
			t.Errorf("publicAddress(%q) = true, want false", in)
		}
	}
	if publicAddress(netip.MustParseAddr("::1.2.3.4")) {
		t.Error("IPv4-compatible address accepted")
	}
}
