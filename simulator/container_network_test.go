// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package simulator

import (
	"slices"
	"testing"

	"github.com/vmware/govmomi/test"
)

func TestNetworkArgs(t *testing.T) {
	const mac = "00:0c:29:31:61:37"
	rootful := containerNetwork{}
	pasta := containerNetwork{rootlessPodman: true, pasta: true}
	slirp := containerNetwork{rootlessPodman: true}

	tests := []struct {
		name     string
		networks []string
		nics     []containerNIC
		rt       containerNetwork
		want     []string
	}{
		{"no MAC", []string{"podman"}, []containerNIC{{}}, pasta, []string{"--network", "podman"}},
		{"named network", []string{"vcsim-test-bridge"}, []containerNIC{{MAC: mac}}, pasta, []string{"--network", "vcsim-test-bridge", "--mac-address", mac}},
		{"rootful default bridge", nil, []containerNIC{{MAC: mac}}, rootful, []string{"--mac-address", mac}},
		{"rootless pasta", nil, []containerNIC{{MAC: mac}}, pasta, []string{"--network", "pasta:--ns-ifname,eth0,--ns-mac-addr," + mac}},
		{"rootless without pasta", nil, []containerNIC{{MAC: mac}}, slirp, nil},
		{"no NICs", nil, nil, pasta, nil},
		{
			"topology NICs in device-key order",
			nil,
			[]containerNIC{
				{Network: "vcsim-wl-v10", MAC: mac, Name: "eth0"},
				{Network: "vcsim-wl-v20", MAC: "00:50:56:00:00:01", Name: "eth1"},
			},
			pasta,
			[]string{
				"--network", "vcsim-wl-v10:mac=" + mac + ",interface_name=eth0",
				"--network", "vcsim-wl-v20:mac=00:50:56:00:00:01,interface_name=eth1",
			},
		},
		{
			// Pasta is for the no-topology single NIC only: a domain network takes its MAC by name.
			"topology NIC on rootless pasta",
			nil,
			[]containerNIC{{Network: "vcsim-wl-v10", MAC: mac, Name: "eth0"}},
			pasta,
			[]string{"--network", "vcsim-wl-v10:mac=" + mac + ",interface_name=eth0"},
		},
		{
			// A further vNIC on a joined network is a sub-interface made after start, not a second join.
			"shared network NICs",
			nil,
			[]containerNIC{
				{Network: "vcsim-wl-v10", MAC: mac, Name: "eth0"},
				{Network: "vcsim-wl-v10", MAC: "00:50:56:00:00:01", Name: "eth1", Parent: "eth0"},
				{Network: "vcsim-wl-v20", MAC: "00:50:56:00:00:02", Name: "eth2"},
			},
			pasta,
			[]string{
				"--network", "vcsim-wl-v10:mac=" + mac + ",interface_name=eth0",
				"--network", "vcsim-wl-v20:mac=00:50:56:00:00:02,interface_name=eth2",
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := networkArgs(tc.networks, tc.nics, tc.rt); !slices.Equal(got, tc.want) {
				t.Errorf("networkArgs(%q, %+v, %+v) = %q, want %q", tc.networks, tc.nics, tc.rt, got, tc.want)
			}
		})
	}
}

// requirePasta skips the test unless a container's primary interface can take
// a vNIC's MAC, which rootless podman does only through pasta (see networkArgs).
func requirePasta(t *testing.T) {
	t.Helper()
	if rt := runtimeNetwork(); rt.rootlessPodman && !rt.pasta {
		t.Skip("requires pasta for the container's MAC under rootless podman, which is not installed")
	}
}

// requireContainerBridge skips the test unless containers can join a bridge
// network, which needs netavark's nftables firewall support on this host.
func requireContainerBridge(t *testing.T) {
	t.Helper()
	test.RequireContainerOptIn(t)
	if !test.HasContainerBridge() {
		t.Skip("this host's netavark/kernel cannot set up bridge firewalling (set NETAVARK_FW=none to skip that setup)")
	}
}

func TestParseLinks(t *testing.T) {
	out := `1: lo: <LOOPBACK,UP,LOWER_UP> mtu 65536 qdisc noqueue qlen 1000\    link/loopback 00:00:00:00:00:00 brd 00:00:00:00:00:00
2: eth0@if4: <BROADCAST,MULTICAST,UP,LOWER_UP,M-DOWN> mtu 1400 qdisc noqueue qlen 1000\    link/ether 00:50:56:AA:BB:01 brd ff:ff:ff:ff:ff:ff
3: eth1@eth0: <BROADCAST,MULTICAST,UP,LOWER_UP> mtu 1400 qdisc noqueue qlen 1000\    link/ether 00:50:56:aa:bb:cc brd ff:ff:ff:ff:ff:ff`
	want := []netLink{
		{name: "lo"},
		{name: "eth0", parent: "if4", mac: "00:50:56:aa:bb:01"},
		{name: "eth1", parent: "eth0", mac: "00:50:56:aa:bb:cc"},
	}
	links := parseLinks(out)
	if !slices.Equal(links, want) {
		t.Fatalf("parseLinks = %+v, want %+v", links, want)
	}
	if l, ok := linkByMAC(links, "00:50:56:AA:BB:CC"); !ok || l.name != "eth1" {
		t.Errorf("linkByMAC = %+v, %v, want eth1", l, ok)
	}
	if _, ok := linkByMAC(links, ""); ok {
		t.Errorf("linkByMAC matched an empty MAC")
	}
	if got := freeLinkName(links); got != "eth2" {
		t.Errorf("freeLinkName = %q, want eth2", got)
	}
}
