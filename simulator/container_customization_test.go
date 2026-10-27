// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package simulator

import (
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/test"
	"github.com/vmware/govmomi/vim25/types"
)

func TestCloudinitPrepPackage(t *testing.T) {
	files := func(entries []tarEntry) map[string]string {
		m := make(map[string]string)
		for _, e := range entries {
			assert.Equal(t, int64(0600), e.header.Mode, e.header.Name)
			m[e.header.Name] = string(e.content)
		}
		return m
	}

	t.Run("metadata and userdata", func(t *testing.T) {
		got := files(cloudinitPrepPackage(&types.CustomizationCloudinitPrep{
			Metadata: "\n instance-id: a\n",
			Userdata: "#cloud-config\nruncmd: []\n\n",
		}, "5026c1e6-0000-0000-0000-000000000001"))

		assert.Equal(t, map[string]string{
			"metadata":  "instance-id: a",
			"userdata":  "#cloud-config\nruncmd: []",
			".cust.cfg": "[CLOUDINIT]\nMETADATA = metadata\nUSERDATA = userdata\n\n[MISC]\nINSTANCE-ID=5026c1e6-0000-0000-0000-000000000001\n",
		}, got)
	})

	t.Run("no userdata", func(t *testing.T) {
		got := files(cloudinitPrepPackage(&types.CustomizationCloudinitPrep{Metadata: "a: b", Userdata: " \n"}, ""))

		assert.Equal(t, map[string]string{
			"metadata":  "a: b",
			".cust.cfg": "[CLOUDINIT]\nMETADATA = metadata\n",
		}, got)
	})

	t.Run("cust.cfg last", func(t *testing.T) {
		entries := cloudinitPrepPackage(&types.CustomizationCloudinitPrep{Metadata: "a: b", Userdata: "c"}, "id")
		assert.Equal(t, ".cust.cfg", entries[len(entries)-1].header.Name)
	})
}

// TestCloudinitPrepContainerDelivery customizes a powered-off container VM and
// checks that each power-on leaves the package where cloud-init's VMware
// datasource reads it, including after the container is restarted with a new
// customization.
func TestCloudinitPrepContainerDelivery(t *testing.T) {
	if !test.HasDocker() {
		t.Skip("requires docker or podman (aliased as docker) on linux")
	}

	env := newVCSIMEnv(t)

	const name = "cloudinitprep-delivery-test"
	spec := types.VirtualMachineConfigSpec{
		Name:  name,
		Files: &types.VirtualMachineFileInfo{VmPathName: "[LocalDS_0] " + name},
		ExtraConfig: []types.BaseOptionValue{
			&types.OptionValue{Key: ContainerBackingOptionKey, Value: `["busybox","sh","-c",` + strconv.Quote(sleepForever) + `]`},
		},
	}
	require.NoError(t, test.ApplyContainerRuntimeDefaults(&spec))

	task, err := env.folder.VmFolder.CreateVM(env.goCtx, spec, env.pool, nil)
	require.NoError(t, err)
	info, err := task.WaitForResult(env.goCtx, nil)
	require.NoError(t, err)
	ref := info.Result.(types.ManagedObjectReference)
	vm := object.NewVirtualMachine(env.client.Client, ref)
	simVM := env.simCtx.Map.Get(ref).(*VirtualMachine)

	customize := func(metadata, userdata string) {
		t.Helper()
		task, err := vm.Customize(env.goCtx, types.CustomizationSpec{
			Identity: &types.CustomizationCloudinitPrep{Metadata: metadata, Userdata: userdata},
		})
		require.NoError(t, err)
		require.NoError(t, task.Wait(env.goCtx))
	}

	read := func(id, file string) string {
		t.Helper()
		out, err := dockerExec(env.goCtx, id, "cat", imcDir+"/"+file)
		require.NoError(t, err, file)
		return out
	}

	customize("instance-id: first\nlocal-hostname: first", "#cloud-config\nhostname: first")
	powerOn(t, env, vm)
	id := containerID(t, env, simVM)

	assert.Equal(t, "instance-id: first\nlocal-hostname: first", read(id, "metadata"))
	assert.Equal(t, "#cloud-config\nhostname: first", read(id, "userdata"))
	assert.Equal(t, "[CLOUDINIT]\nMETADATA = metadata\nUSERDATA = userdata\n\n[MISC]\nINSTANCE-ID="+simVM.Config.InstanceUuid,
		read(id, "cust.cfg"))

	out, err := dockerExec(env.goCtx, id, "sh", "-c", "stat -c '%a %n' "+imcDir+" "+imcDir+"/*; ls -A "+imcDir)
	require.NoError(t, err)
	assert.Equal(t, "700 "+imcDir+"\n600 "+imcDir+"/cust.cfg\n600 "+imcDir+"/metadata\n600 "+imcDir+"/userdata\ncust.cfg\nmetadata\nuserdata", out)

	var status string
	env.simCtx.WithLock(simVM, func() {
		status = simVM.Guest.CustomizationInfo.CustomizationStatus
	})
	assert.Equal(t, string(types.GuestInfoCustomizationStatusTOOLSDEPLOYPKG_SUCCEEDED), status)

	// cloud-init deletes the directory once it has read the package.
	_, err = dockerExec(env.goCtx, id, "rm", "-rf", imcDir)
	require.NoError(t, err)

	powerOff(t, env, vm)
	customize("instance-id: second", "")
	powerOn(t, env, vm)
	require.Equal(t, id, containerID(t, env, simVM), "the container is restarted, not recreated")

	assert.Equal(t, "instance-id: second", read(id, "metadata"))
	assert.Equal(t, "[CLOUDINIT]\nMETADATA = metadata\n\n[MISC]\nINSTANCE-ID="+simVM.Config.InstanceUuid, read(id, "cust.cfg"))
	_, err = dockerExec(env.goCtx, id, "test", "-e", imcDir+"/userdata")
	assert.Error(t, err, "an empty userdata is not delivered")

	// A power-on without a pending customization delivers nothing.
	_, err = dockerExec(env.goCtx, id, "rm", "-rf", imcDir)
	require.NoError(t, err)
	powerOff(t, env, vm)
	powerOn(t, env, vm)
	_, err = dockerExec(env.goCtx, id, "test", "-e", imcDir)
	assert.Error(t, err, "no package without a customization")
}

// TestCloudinitPrepContainerDeliveryFailure checks that a guest that cannot
// take the package fails the customization, not the power-on.
func TestCloudinitPrepContainerDeliveryFailure(t *testing.T) {
	if !test.HasDocker() {
		t.Skip("requires docker or podman (aliased as docker) on linux")
	}

	env := newVCSIMEnv(t)

	const name = "cloudinitprep-failure-test"
	spec := types.VirtualMachineConfigSpec{
		Name:  name,
		Files: &types.VirtualMachineFileInfo{VmPathName: "[LocalDS_0] " + name},
		ExtraConfig: []types.BaseOptionValue{
			&types.OptionValue{Key: ContainerBackingOptionKey, Value: `["busybox","sh","-c",` + strconv.Quote(sleepForever) + `]`},
			// A read-only mount at the package's directory refuses it.
			&types.OptionValue{Key: "RUN.volume.imc", Value: t.TempDir() + ":" + imcDir + ":ro"},
		},
	}
	require.NoError(t, test.ApplyContainerRuntimeDefaults(&spec))

	task, err := env.folder.VmFolder.CreateVM(env.goCtx, spec, env.pool, nil)
	require.NoError(t, err)
	info, err := task.WaitForResult(env.goCtx, nil)
	require.NoError(t, err)
	ref := info.Result.(types.ManagedObjectReference)
	vm := object.NewVirtualMachine(env.client.Client, ref)
	simVM := env.simCtx.Map.Get(ref).(*VirtualMachine)

	task, err = vm.Customize(env.goCtx, types.CustomizationSpec{
		Identity: &types.CustomizationCloudinitPrep{Metadata: "instance-id: a"},
	})
	require.NoError(t, err)
	require.NoError(t, task.Wait(env.goCtx))

	powerOn(t, env, vm)

	env.simCtx.WithLock(simVM, func() {
		assert.Equal(t, types.VirtualMachinePowerStatePoweredOn, simVM.Runtime.PowerState)
		assert.Equal(t, string(types.GuestInfoCustomizationStatusTOOLSDEPLOYPKG_FAILED), simVM.Guest.CustomizationInfo.CustomizationStatus)
		assert.Contains(t, simVM.Guest.CustomizationInfo.ErrorMsg, imcDir)
		assert.Empty(t, simVM.Config.Tools.PendingCustomization)
	})
}
