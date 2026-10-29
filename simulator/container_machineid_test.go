// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package simulator

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/test"
	"github.com/vmware/govmomi/vim25/types"
)

// createContainerVM creates, but does not power on, a container VM whose
// config.uuid is biosUUID, or the one vcsim derives if biosUUID is "".
func createContainerVM(t *testing.T, env *vcSimEnv, name, biosUUID string, extraConfig ...types.BaseOptionValue) (*object.VirtualMachine, *VirtualMachine) {
	t.Helper()

	spec := types.VirtualMachineConfigSpec{
		Name:  name,
		Uuid:  biosUUID,
		Files: &types.VirtualMachineFileInfo{VmPathName: "[LocalDS_0] " + name},
		ExtraConfig: append([]types.BaseOptionValue{
			&types.OptionValue{Key: ContainerBackingOptionKey, Value: `["alpine","sh","-c","` + sleepForever + `"]`},
		}, extraConfig...),
	}
	require.NoError(t, test.ApplyContainerRuntimeDefaults(&spec))

	task, err := env.folder.VmFolder.CreateVM(env.goCtx, spec, env.pool, nil)
	require.NoError(t, err)
	info, err := task.WaitForResult(env.goCtx, nil)
	require.NoError(t, err)

	ref := info.Result.(types.ManagedObjectReference)
	return object.NewVirtualMachine(env.client.Client, ref), env.simCtx.Map.Get(ref).(*VirtualMachine)
}

// machineIDDir returns the host directory holding vm's machine-id file.
func machineIDDir(env *vcSimEnv, vm *VirtualMachine) string {
	var dir string
	env.simCtx.WithLock(vm, func() {
		if vm.svm != nil {
			dir = vm.svm.machineIDDir
		}
	})
	return dir
}

// requireMachineID checks that the guest of vm reads want as its machine ID
// at each of machineIDPaths.
func requireMachineID(t *testing.T, env *vcSimEnv, vm *VirtualMachine, want string) {
	t.Helper()

	id := containerID(t, env, vm)
	for _, p := range machineIDPaths {
		out, err := dockerExec(env.goCtx, id, "cat", p)
		require.NoError(t, err, p)
		require.Equal(t, want, out, p)
	}
}

// TestContainerVM_MachineID checks that each container VM's guest reads its
// own BIOS UUID as its machine ID, from first power-on, after config.uuid is
// reconfigured, and that the host file goes with the VM.
func TestContainerVM_MachineID(t *testing.T) {
	test.RequireContainerOptIn(t)
	if !test.HasDocker() {
		t.Skip("requires docker or podman (aliased as docker) on linux")
	}

	env := newVCSIMEnv(t)

	const biosUUID = "4215A0C3-1D2E-4F5A-8B6C-7D8E9FA0B1C2"
	vmSet, simSet := createContainerVM(t, env, "machine-id-spec-uuid", biosUUID)
	vmDerived, simDerived := createContainerVM(t, env, "machine-id-derived-uuid", "")
	powerOn(t, env, vmSet)
	powerOn(t, env, vmDerived)

	requireMachineID(t, env, simSet, "4215a0c31d2e4f5a8b6c7d8e9fa0b1c2")
	requireMachineID(t, env, simDerived, strings.ReplaceAll(simDerived.Config.Uuid, "-", ""))

	// The container outlives a power off, so a reconfigured config.uuid
	// reaches the guest through the same mount.
	powerOff(t, env, vmSet)
	const newUUID = "42150000-1111-2222-3333-444455556666"
	task, err := vmSet.Reconfigure(env.goCtx, types.VirtualMachineConfigSpec{Uuid: newUUID})
	require.NoError(t, err)
	require.NoError(t, task.Wait(env.goCtx))
	powerOn(t, env, vmSet)
	requireMachineID(t, env, simSet, "42150000111122223333444455556666")

	dir := machineIDDir(env, simSet)
	require.NotEmpty(t, dir)
	powerOff(t, env, vmSet)
	task, err = vmSet.Destroy(env.goCtx)
	require.NoError(t, err)
	require.NoError(t, task.Wait(env.goCtx))
	_, err = os.Stat(dir)
	require.True(t, os.IsNotExist(err), "%s not removed with its VM: %v", dir, err)

	powerOff(t, env, vmDerived)
}

// TestContainerVM_MachineIDOptOut checks that RUN.machineId=false, and a
// RUN.volume at a machine ID path, leave the image's file in place.
func TestContainerVM_MachineIDOptOut(t *testing.T) {
	test.RequireContainerOptIn(t)
	if !test.HasDocker() {
		t.Skip("requires docker or podman (aliased as docker) on linux")
	}

	env := newVCSIMEnv(t)

	vmOff, simOff := createContainerVM(t, env, "machine-id-opt-out", "",
		&types.OptionValue{Key: "RUN.machineId", Value: "false"})
	powerOn(t, env, vmOff)
	require.Empty(t, machineIDDir(env, simOff))
	for _, p := range machineIDPaths {
		_, err := dockerExec(env.goCtx, containerID(t, env, simOff), "test", "-e", p)
		require.Error(t, err, "%s exists in the alpine image or was mounted", p)
	}
	powerOff(t, env, vmOff)

	own := t.TempDir() + "/machine-id"
	require.NoError(t, os.WriteFile(own, []byte("0123456789abcdef0123456789abcdef\n"), 0644))
	require.NoError(t, os.Chmod(own, 0644))
	vmOwn, simOwn := createContainerVM(t, env, "machine-id-own-volume", "",
		&types.OptionValue{Key: "RUN.volume.machineid", Value: own + ":/etc/machine-id:ro"})
	powerOn(t, env, vmOwn)
	id := containerID(t, env, simOwn)
	out, err := dockerExec(env.goCtx, id, "cat", "/etc/machine-id")
	require.NoError(t, err)
	require.Equal(t, "0123456789abcdef0123456789abcdef", out)
	out, err = dockerExec(env.goCtx, id, "cat", "/var/lib/dbus/machine-id")
	require.NoError(t, err)
	require.Equal(t, strings.ReplaceAll(simOwn.Config.Uuid, "-", ""), out)
	powerOff(t, env, vmOwn)
}
