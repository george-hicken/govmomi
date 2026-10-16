// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package simulator

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/test"
	"github.com/vmware/govmomi/vim25/types"
)

// TestVMCI_ToolboxDaemon runs the toolbox binary as the vmtoolsd daemon in a
// container-backed VM, and checks that the VM's tools status, tools version
// and guest network identity come from what it reports, through power
// operations.
func TestVMCI_ToolboxDaemon(t *testing.T) {
	if !test.HasDocker() {
		t.Skip("requires docker or podman (aliased as docker) on linux")
	}
	requirePasta(t)

	env := newVCSIMEnv(t)
	name := "vmci-toolbox-daemon"
	spec := types.VirtualMachineConfigSpec{
		Name:  name,
		Files: &types.VirtualMachineFileInfo{VmPathName: "[LocalDS_0] " + name},
		DeviceChange: []types.BaseVirtualDeviceConfigSpec{&types.VirtualDeviceConfigSpec{
			Operation: types.VirtualDeviceConfigSpecOperationAdd,
			Device: &types.VirtualVmxnet3{VirtualVmxnet: types.VirtualVmxnet{VirtualEthernetCard: types.VirtualEthernetCard{
				VirtualDevice: types.VirtualDevice{
					Key:     -1,
					Backing: &types.VirtualEthernetCardNetworkBackingInfo{VirtualDeviceDeviceBackingInfo: types.VirtualDeviceDeviceBackingInfo{DeviceName: "VM Network"}},
				},
			}}},
		}},
		ExtraConfig: []types.BaseOptionValue{
			&types.OptionValue{Key: ContainerBackingOptionKey, Value: `["alpine","sh","-c","/usr/bin/vmtoolsd & ` + sleepForever + `"]`},
			&types.OptionValue{Key: "RUN.vmci", Value: "true"},
			&types.OptionValue{Key: "RUN.volume.vmtoolsd", Value: toolboxBinary(t) + ":/usr/bin/vmtoolsd:ro"},
		},
	}
	require.NoError(t, test.ApplyContainerRuntimeDefaults(&spec))

	task, err := env.folder.VmFolder.CreateVM(env.goCtx, spec, env.pool, nil)
	require.NoError(t, err)
	info, err := task.WaitForResult(env.goCtx, nil)
	require.NoError(t, err)
	ref := info.Result.(types.ManagedObjectReference)
	vmObj := object.NewVirtualMachine(env.client.Client, ref)
	vm := env.simCtx.Map.Get(ref).(*VirtualMachine)
	powerOn(t, env, vmObj)

	requireToolsRunningStatus(t, env, vm, guestToolsRunning)

	card := firstEthernetCard(t, vm)
	ctx, cancel := context.WithTimeout(env.goCtx, 30*time.Second)
	defer cancel()
	for {
		var ip string
		var net []types.GuestNicInfo
		var reported bool
		env.simCtx.WithLock(vm, func() {
			ip = vm.Guest.IpAddress
			net = vm.Guest.Net
			reported = vm.svm.toolsNicInfo.Load()
		})
		// Until the toolbox reports, the container's observed address is.
		if reported && ip != "" {
			require.NotEmpty(t, net)
			require.Equal(t, card.Key, net[0].DeviceConfigId)
			require.Equal(t, card.MacAddress, net[0].MacAddress)
			require.Contains(t, net[0].IpAddress, ip, "guest.ipAddress is the first NIC's")
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("no guest.ipAddress reported by the toolbox")
		case <-time.After(100 * time.Millisecond):
		}
	}

	// The toolbox registers its version before it reports its NICs, but after
	// it reports its description, which makes it running.
	env.simCtx.WithLock(vm, func() {
		require.Equal(t, string(types.VirtualMachineToolsVersionStatusGuestToolsUnmanaged), vm.Guest.ToolsVersionStatus2)
	})

	task, err = vmObj.Suspend(env.goCtx)
	require.NoError(t, err)
	require.NoError(t, task.Wait(env.goCtx))
	requireToolsRunningStatus(t, env, vm, guestToolsNotRunning)

	powerOn(t, env, vmObj)
	requireToolsRunningStatus(t, env, vm, guestToolsRunning)

	task, err = vmObj.Reset(env.goCtx)
	require.NoError(t, err)
	require.NoError(t, task.Wait(env.goCtx))
	requireToolsRunningStatus(t, env, vm, guestToolsRunning)

	powerOff(t, env, vmObj)
	requireToolsRunningStatus(t, env, vm, guestToolsNotRunning)
}
