package councilproxy

import (
	"net/netip"
	"testing"
)

// One line per row of the IANA IPv4 and IPv6 Special-Purpose Address
// Registries (both last updated 2025-10-09), in registry order, plus the
// ranges the filter refuses outside them (multicast, IPv6 outside 2000::/3).
// Each line has an address inside the range and one just outside it; for a
// row nested in a larger refused block, the outside address is the one just
// outside that block. Every row is refused, the globally reachable ones too:
// none of them serves a provider API.
func TestPublicAddressSpecialPurposeRanges(t *testing.T) {
	for _, tc := range []struct{ name, inside, outside string }{
		{"0.0.0.0/8 this network", "0.255.255.255", "1.0.0.0"},
		{"0.0.0.0/32 this host", "0.0.0.0", "1.0.0.0"},
		{"10.0.0.0/8 private", "10.255.255.255", "11.0.0.0"},
		{"100.64.0.0/10 shared address space", "100.127.255.255", "100.128.0.0"},
		{"100.64.0.0/10 lower edge", "100.64.0.0", "100.63.255.255"},
		{"127.0.0.0/8 loopback", "127.255.255.255", "128.0.0.1"},
		{"169.254.0.0/16 link local", "169.254.169.254", "169.255.0.1"},
		{"172.16.0.0/12 private", "172.31.255.255", "172.32.0.1"},
		{"192.0.0.0/24 IETF protocol assignments", "192.0.0.255", "192.0.1.1"},
		{"192.0.0.0/29 IPv4 service continuity", "192.0.0.7", "192.0.1.1"},
		{"192.0.0.8/32 IPv4 dummy address", "192.0.0.8", "192.0.1.1"},
		{"192.0.0.9/32 PCP anycast", "192.0.0.9", "192.0.1.1"},
		{"192.0.0.10/32 TURN anycast", "192.0.0.10", "192.0.1.1"},
		{"192.0.0.170/32 NAT64 discovery", "192.0.0.170", "192.0.1.1"},
		{"192.0.0.171/32 NAT64 discovery", "192.0.0.171", "192.0.1.1"},
		{"192.0.2.0/24 documentation", "192.0.2.255", "192.0.3.1"},
		{"192.31.196.0/24 AS112-v4", "192.31.196.255", "192.31.197.1"},
		{"192.31.196.0/24 lower edge", "192.31.196.0", "192.31.195.255"},
		{"192.52.193.0/24 AMT", "192.52.193.255", "192.52.194.1"},
		{"192.52.193.0/24 lower edge", "192.52.193.0", "192.52.192.255"},
		{"192.88.99.0/24 deprecated 6to4 relay", "192.88.99.1", "192.88.100.1"},
		{"192.88.99.2/32 6a44 relay anycast", "192.88.99.2", "192.88.100.1"},
		{"192.168.0.0/16 private", "192.168.255.255", "192.169.0.1"},
		{"192.175.48.0/24 direct delegation AS112", "192.175.48.255", "192.175.49.1"},
		{"192.175.48.0/24 lower edge", "192.175.48.0", "192.175.47.255"},
		{"198.18.0.0/15 benchmarking", "198.19.255.255", "198.20.0.1"},
		{"198.51.100.0/24 documentation", "198.51.100.255", "198.51.101.1"},
		{"203.0.113.0/24 documentation", "203.0.113.255", "203.0.114.1"},
		{"224.0.0.0/4 multicast", "239.255.255.255", "223.255.255.255"},
		{"240.0.0.0/4 reserved", "240.0.0.1", "223.255.255.255"},
		{"255.255.255.255/32 limited broadcast", "255.255.255.255", "223.255.255.255"},
		{"::1/128 loopback", "::1", "2000::1"},
		{"::/128 unspecified", "::", "2000::1"},
		{"64:ff9b::/96 NAT64", "64:ff9b::808:808", "2000::1"},
		{"64:ff9b:1::/48 local NAT64", "64:ff9b:1:ffff::1", "2000::1"},
		{"100::/64 discard only", "100::1", "2000::1"},
		{"100:0:0:1::/64 dummy prefix", "100:0:0:1::1", "2000::1"},
		{"2001::/23 IETF protocol assignments", "2001:1ff:ffff::1", "2001:200::1"},
		{"2001::/32 Teredo", "2001:0:ffff::1", "2001:200::1"},
		{"2001:1::1/128 PCP anycast", "2001:1::1", "2001:200::1"},
		{"2001:1::2/128 TURN anycast", "2001:1::2", "2001:200::1"},
		{"2001:1::3/128 DNS-SD SRP anycast", "2001:1::3", "2001:200::1"},
		{"2001:2::/48 benchmarking", "2001:2::1", "2001:200::1"},
		{"2001:3::/32 AMT", "2001:3:ffff::1", "2001:200::1"},
		{"2001:4:112::/48 AS112-v6", "2001:4:112::1", "2001:200::1"},
		{"2001:10::/28 deprecated ORCHID", "2001:1f::1", "2001:200::1"},
		{"2001:20::/28 ORCHIDv2", "2001:2f::1", "2001:200::1"},
		{"2001:30::/28 drone DETs", "2001:3f::1", "2001:200::1"},
		{"2001:db8::/32 documentation", "2001:db8:ffff::1", "2001:db9::1"},
		{"2002::/16 6to4", "2002:ffff::1", "2003::1"},
		{"2620:4f:8000::/48 direct delegation AS112", "2620:4f:8000:ffff::1", "2620:4f:8001::1"},
		{"2620:4f:8000::/48 lower edge", "2620:4f:8000::", "2620:4f:7fff:ffff::1"},
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

// The row ::ffff:0:0/96 (IPv4-mapped addresses) is a deliberate exception to
// "every row is refused": publicAddress converts a mapped address to IPv4
// first, so a mapped address is refused exactly when its IPv4 form is. The
// mapped form of a public address is therefore allowed, and the mapped form
// of a refused one is refused.
func TestPublicAddressIPv4MappedFollowsIPv4Form(t *testing.T) {
	for _, tc := range []struct {
		name, addr string
		want       bool
	}{
		{"mapped public address is allowed (documented exception)", "::ffff:1.2.3.4", true},
		{"mapped private address is refused", "::ffff:10.0.0.1", false},
		{"mapped loopback is refused", "::ffff:127.0.0.1", false},
		{"mapped link local is refused", "::ffff:169.254.169.254", false},
		{"mapped documentation address is refused", "::ffff:192.0.2.1", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := publicAddress(netip.MustParseAddr(tc.addr)); got != tc.want {
				t.Errorf("publicAddress(%q) = %v, want %v", tc.addr, got, tc.want)
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
