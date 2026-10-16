// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package simulator

import (
	"context"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/vmware/govmomi/property"
	"github.com/vmware/govmomi/toolbox"
	"github.com/vmware/govmomi/vim25/types"
)

// firstEthernetCard returns vm's first virtual NIC.
func firstEthernetCard(t *testing.T, vm *VirtualMachine) *types.VirtualEthernetCard {
	t.Helper()
	for _, d := range vm.Config.Hardware.Device {
		if c, ok := d.(types.BaseVirtualEthernetCard); ok {
			return c.GetVirtualEthernetCard()
		}
	}
	t.Fatalf("%s has no virtual NIC", vm.Name)
	return nil
}

func nicEntry(cidr string) toolbox.IPAddressEntry {
	ip, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		panic(err)
	}
	size, _ := ipnet.Mask.Size()
	origin, status := int32(2), int32(1) // IAO_MANUAL, IAS_PREFERRED
	return toolbox.IPAddressEntry{
		Address:      toolbox.NewTypedIPAddress(ip),
		PrefixLength: uint32(size),
		Origin:       &origin,
		Status:       &status,
	}
}

// waitGuestProperty waits, through PropertyCollector, for vmRef's property
// name to equal want.
func waitGuestProperty(t *testing.T, env *vcSimEnv, vmRef types.ManagedObjectReference, name string, want any) {
	t.Helper()

	ctx, cancel := context.WithTimeout(env.goCtx, 10*time.Second)
	defer cancel()

	var got any
	err := property.Wait(ctx, env.pc, vmRef, []string{name}, func(changes []types.PropertyChange) bool {
		for _, c := range changes {
			got = c.Val
		}
		return got == want
	})
	require.NoError(t, err, "%s: got %v, want %v", name, got, want)
}

func TestSetGuestInfo_NicInfoMatchedByMAC(t *testing.T) {
	env := newVCSIMEnv(t)
	vm := allVMs(t, env)[0]
	srv, ch := serveGuestRPC(t, env, vm)

	card := firstEthernetCard(t, vm)
	strayMAC := "00:11:22:33:44:55"
	hostName, domain := "node-1", "corp.example"
	gw := toolbox.NewTypedIPAddress(net.ParseIP("10.0.2.1"))

	info := toolbox.NewGuestNicInfo()
	info.V3.Nics = []toolbox.GuestNicV3{
		{MacAddress: strayMAC, IPs: []toolbox.IPAddressEntry{nicEntry("192.168.9.9/24")}},
		{
			// vmtoolsd reports MACs in lower case; vSphere may not.
			MacAddress: strings.ToUpper(card.MacAddress),
			IPs: []toolbox.IPAddressEntry{
				nicEntry("169.254.10.1/16"),
				nicEntry("fd00::100/64"),
				nicEntry("10.0.2.100/24"),
				nicEntry("10.0.2.99/32"),
			},
		},
	}
	info.V3.Routes = []toolbox.InetCidrRouteEntry{
		{Dest: toolbox.NewTypedIPAddress(net.IPv4zero), NextHop: &gw, IfIndex: 1},
		{Dest: toolbox.NewTypedIPAddress(net.ParseIP("192.168.9.0")), PrefixLength: 24, IfIndex: 0},
		{Dest: toolbox.NewTypedIPAddress(net.ParseIP("10.9.0.0")), PrefixLength: 16, IfIndex: 7},
	}
	info.V3.DNSConfigInfo = &toolbox.DNSConfigInfo{
		HostName:   &hostName,
		DomainName: &domain,
		Servers:    []toolbox.TypedIPAddress{toolbox.NewTypedIPAddress(net.ParseIP("10.0.2.3"))},
		Search:     []string{domain},
	}
	payload, err := toolbox.EncodeXDR(info)
	require.NoError(t, err)

	_, err = ch.Request(toolbox.GuestInfoCommand(toolbox.GuestInfoIPAddressV2, payload))
	require.NoError(t, err)
	_, err = ch.Request([]byte("SetGuestInfo  1 " + hostName + "\x00"))
	require.NoError(t, err)

	waitGuestProperty(t, env, vm.Self, "guest.ipAddress", "10.0.2.100")
	waitGuestProperty(t, env, vm.Self, "guest.hostName", hostName)
	require.True(t, srv.reported == nil || srv.reported.Load())

	env.simCtx.WithLock(vm, func() {
		require.Equal(t, "10.0.2.100", vm.Summary.Guest.IpAddress)
		require.Equal(t, hostName, vm.Summary.Guest.HostName)

		nets := vm.Guest.Net
		require.Len(t, nets, 2, "the virtual NIC, then the guest NIC matching none")

		primary := nets[0]
		require.Equal(t, card.Key, primary.DeviceConfigId)
		require.Equal(t, card.MacAddress, primary.MacAddress)
		require.True(t, primary.Connected)
		require.NotEmpty(t, primary.Network)
		require.Equal(t, []string{"169.254.10.1", "fd00::100", "10.0.2.100", "10.0.2.99"}, primary.IpAddress)
		require.NotNil(t, primary.IpConfig)
		require.Equal(t, types.NetIpConfigInfoIpAddress{
			IpAddress:    "10.0.2.100",
			PrefixLength: 24,
			Origin:       string(types.NetIpConfigInfoIpAddressOriginManual),
			State:        string(types.NetIpConfigInfoIpAddressStatusPreferred),
		}, primary.IpConfig.IpAddress[2])

		stray := nets[1]
		require.Equal(t, int32(-1), stray.DeviceConfigId)
		require.Equal(t, strayMAC, stray.MacAddress)
		require.False(t, stray.Connected)
		require.Equal(t, []string{"192.168.9.9"}, stray.IpAddress)

		require.Len(t, vm.Guest.IpStack, 1)
		stack := vm.Guest.IpStack[0]
		require.Equal(t, &types.NetDnsConfigInfo{
			HostName:     hostName,
			DomainName:   domain,
			IpAddress:    []string{"10.0.2.3"},
			SearchDomain: []string{domain},
		}, stack.DnsConfig)
		require.Equal(t, []types.NetIpRouteConfigInfoIpRoute{
			{Network: "0.0.0.0", Gateway: types.NetIpRouteConfigInfoGateway{IpAddress: "10.0.2.1", Device: "0"}},
			{Network: "192.168.9.0", PrefixLength: 24, Gateway: types.NetIpRouteConfigInfoGateway{Device: "1"}},
		}, stack.IpRouteConfig.IpRoute, "routes renumbered to guest.net; one on an unknown NIC dropped")
	})
}

func TestSetGuestInfo_Replies(t *testing.T) {
	srv := &guestRPCServer{}

	v2, err := toolbox.EncodeXDR(&toolbox.GuestNicInfo{Version: 2})
	require.NoError(t, err)

	tests := []struct {
		name, cmd, reply string
	}{
		{"unknown type is accepted", "SetGuestInfo  7 12345\x00", "1 "},
		{"nicinfo V2 is accepted and ignored", string(toolbox.GuestInfoCommand(toolbox.GuestInfoIPAddressV2, v2)), "1 "},
		{"undecodable nicinfo", string(toolbox.GuestInfoCommand(toolbox.GuestInfoIPAddressV3, []byte{0, 0})), "0 Invalid nicinfo"},
		{"no value", "SetGuestInfo  1", "0 Bad syntax for SetGuestInfo"},
		{"bad type", "SetGuestInfo  x y", "0 Bad syntax for SetGuestInfo"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.reply, srv.dispatch(nil, tt.cmd))
		})
	}
}

// TestSetGuestInfo_ToolboxService runs a toolbox Service against the server,
// as the guest's toolbox daemon does, and checks that its first push reaches
// guest.hostName.
func TestSetGuestInfo_ToolboxService(t *testing.T) {
	env := newVCSIMEnv(t)
	vm := allVMs(t, env)[0]
	srv, _ := serveGuestRPC(t, env, vm)

	name, err := os.Hostname()
	require.NoError(t, err)

	service := toolbox.NewService(toolbox.NewNoopChannelIn(), toolbox.NewUnixChannelOut(srv.path))
	service.GuestInfoInterval = time.Hour // only the push on start
	require.NoError(t, service.Start())
	t.Cleanup(func() {
		service.Stop()
		service.Wait()
	})

	waitGuestProperty(t, env, vm.Self, "guest.hostName", name)
}

// TestSyncNetworkConfig_ToolsTakePrecedence checks D-95-31's order: the
// container's network settings are reported until the guest's tools report
// its NICs, and the power state is still taken from the container after.
func TestSyncNetworkConfig_ToolsTakePrecedence(t *testing.T) {
	m := VPX()
	defer m.Remove()
	require.NoError(t, m.Create())

	ctx := &Context{Context: context.Background(), Map: m.Service.Context.Map}
	vm := ctx.Map.All("VirtualMachine")[0].(*VirtualMachine)
	svm := &simVM{vm: vm}

	detail := func(ip string, paused bool) *containerDetails {
		var d containerDetails
		d.State.Running = true
		d.State.Paused = paused
		d.Config.Hostname = "container-" + ip
		d.NetworkSettings.Networks = map[string]networkSettings{
			"bridge": {IPAddress: ip, IPPrefixLen: 16, Gateway: "172.17.0.1", MacAddress: "00:50:56:aa:bb:cc"},
		}
		return &d
	}

	require.NoError(t, svm.syncNetworkConfigToVMGuestProperties(ctx, nil, detail("172.17.0.5", false)))
	require.Equal(t, "172.17.0.5", vm.Guest.IpAddress)
	require.Equal(t, "container-172.17.0.5", vm.Guest.HostName)

	svm.toolsNicInfo.Store(true)
	ctx.AutoUpdate(vm, func() {
		vm.Guest.IpAddress = "10.0.2.100"
		vm.Guest.HostName = "guest"
		vm.Guest.Net = nil
	})

	require.NoError(t, svm.syncNetworkConfigToVMGuestProperties(ctx, nil, detail("172.17.0.6", true)))
	require.Equal(t, "10.0.2.100", vm.Guest.IpAddress)
	require.Equal(t, "guest", vm.Guest.HostName)
	require.Nil(t, vm.Guest.Net)
	require.Equal(t, types.VirtualMachinePowerStateSuspended, vm.Runtime.PowerState)
}
