// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package simulator

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/vmware/govmomi/property"
	"github.com/vmware/govmomi/toolbox"
	"github.com/vmware/govmomi/vim25/types"
)

// TestSetGuestInfo_IPv4PublishedAfterLinkLocal reports a link-local IPv6
// address, then a global IPv4 address, for the same NIC over one channel. A
// PropertyCollector waiter on guest.net and guest.ipAddress, opened first and
// kept open as vm-operator keeps its WaitForUpdatesEx open, must see the IPv4
// address in both properties.
func TestSetGuestInfo_IPv4PublishedAfterLinkLocal(t *testing.T) {
	env := newVCSIMEnv(t)
	vm := allVMs(t, env)[0]
	_, ch := serveGuestRPC(t, env, vm)
	card := firstEthernetCard(t, vm)

	report := func(entries ...toolbox.IPAddressEntry) {
		t.Helper()
		info := toolbox.NewGuestNicInfo()
		info.V3.Nics = []toolbox.GuestNicV3{{MacAddress: card.MacAddress, IPs: entries}}
		payload, err := toolbox.EncodeXDR(info)
		require.NoError(t, err)
		_, err = ch.Request(toolbox.GuestInfoCommand(toolbox.GuestInfoIPAddressV2, payload))
		require.NoError(t, err)
	}

	linkLocal := nicEntry("fe80::20c:29ff:fe30:3461/64")
	global := nicEntry("10.0.2.101/24")

	ctx, cancel := context.WithTimeout(env.goCtx, 10*time.Second)
	defer cancel()

	var net []types.GuestNicInfo
	var ip string
	done := make(chan error, 1)
	go func() {
		done <- property.Wait(ctx, env.pc, vm.Self, []string{"guest.net", "guest.ipAddress"}, func(changes []types.PropertyChange) bool {
			for _, c := range changes {
				switch c.Name {
				case "guest.net":
					if v, ok := c.Val.(types.ArrayOfGuestNicInfo); ok {
						net = v.GuestNicInfo
					}
				case "guest.ipAddress":
					ip, _ = c.Val.(string)
				}
			}
			return ip == "10.0.2.101"
		})
	}()

	report(linkLocal)
	report(linkLocal, global)

	require.NoError(t, <-done, "guest.ipAddress: got %q, guest.net=%+v", ip, net)
	require.Len(t, net, 1)
	require.Equal(t, []string{"fe80::20c:29ff:fe30:3461", "10.0.2.101"}, net[0].IpAddress)

	env.simCtx.WithLock(vm, func() {
		require.Equal(t, "10.0.2.101", vm.Guest.IpAddress)
		require.Equal(t, "10.0.2.101", vm.Summary.Guest.IpAddress)
	})
}
