// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package simulator

import (
	"slices"
	"testing"
)

func TestNetworkArgs(t *testing.T) {
	const mac = "00:0c:29:31:61:37"
	rootful := containerNetwork{}
	pasta := containerNetwork{rootlessPodman: true, pasta: true}
	slirp := containerNetwork{rootlessPodman: true}

	tests := []struct {
		name     string
		networks []string
		mac      string
		rt       containerNetwork
		want     []string
	}{
		{"no MAC", []string{"podman"}, "", pasta, []string{"--network", "podman"}},
		{"named network", []string{"vcsim-test-bridge"}, mac, pasta, []string{"--network", "vcsim-test-bridge", "--mac-address", mac}},
		{"rootful default bridge", nil, mac, rootful, []string{"--mac-address", mac}},
		{"rootless pasta", nil, mac, pasta, []string{"--network", "pasta:--ns-ifname,eth0,--ns-mac-addr," + mac}},
		{"rootless without pasta", nil, mac, slirp, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := networkArgs(tc.networks, tc.mac, tc.rt); !slices.Equal(got, tc.want) {
				t.Errorf("networkArgs(%q, %q, %+v) = %q, want %q", tc.networks, tc.mac, tc.rt, got, tc.want)
			}
		})
	}
}
