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
	if !test.HasContainerBridge() {
		t.Skip("this host's netavark/kernel cannot set up bridge firewalling (set NETAVARK_FW=none to skip that setup)")
	}
}
