// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package simulator

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/test"
	"github.com/vmware/govmomi/vim25/types"
)

// These tests run the govmomi toolbox binary, as vmtoolsd and vmware-rpctool,
// inside container-backed VMs with RUN.vmci=true. They need a docker CLI,
// which may be podman's.

// vmciSystemdInitImage boots systemd with a unit that writes
// guestinfo.vmci-systemd-test=pass through vmware-rpctool.
const vmciSystemdInitImage = "localhost/vmci-systemd-init:latest"

var (
	vmciSystemdInitOnce sync.Once
	vmciSystemdInitErr  error
)

// buildVMCISystemdInitImage builds vmciSystemdInitImage once per test binary.
// It always builds, relying on the image build cache, so that a changed
// Dockerfile cannot be shadowed by an old image.
func buildVMCISystemdInitImage(t *testing.T) {
	t.Helper()

	vmciSystemdInitOnce.Do(func() {
		out, err := exec.Command("docker", "build", "-t", vmciSystemdInitImage, "testdata/vmci-systemd-init").CombinedOutput()
		if err != nil {
			vmciSystemdInitErr = fmt.Errorf("%w\n%s", err, out)
		}
	})
	require.NoError(t, vmciSystemdInitErr, "docker build %s", vmciSystemdInitImage)
}

// toolboxBinary returns the toolbox binary RUN.vmci mounts, failing the test
// if it cannot be built.
func toolboxBinary(t *testing.T) string {
	t.Helper()

	path, err := buildToolbox()
	require.NoError(t, err)

	return path
}

// powerOnContainerVM creates a VM with extraConfig and powers it on.
func powerOnContainerVM(t *testing.T, env *vcSimEnv, name string, extraConfig ...types.BaseOptionValue) (*object.VirtualMachine, *VirtualMachine) {
	t.Helper()

	spec := types.VirtualMachineConfigSpec{
		Name:        name,
		Files:       &types.VirtualMachineFileInfo{VmPathName: "[LocalDS_0] " + name},
		ExtraConfig: extraConfig,
	}
	require.NoError(t, test.ApplyContainerRuntimeDefaults(&spec))

	task, err := env.folder.VmFolder.CreateVM(env.goCtx, spec, env.pool, nil)
	require.NoError(t, err)
	info, err := task.WaitForResult(env.goCtx, nil)
	require.NoError(t, err)

	ref := info.Result.(types.ManagedObjectReference)
	vm := object.NewVirtualMachine(env.client.Client, ref)
	powerOn(t, env, vm)

	return vm, env.simCtx.Map.Get(ref).(*VirtualMachine)
}

func powerOn(t *testing.T, env *vcSimEnv, vm *object.VirtualMachine) {
	t.Helper()

	task, err := vm.PowerOn(env.goCtx)
	require.NoError(t, err)
	require.NoError(t, task.Wait(env.goCtx))
}

func powerOff(t *testing.T, env *vcSimEnv, vm *object.VirtualMachine) {
	t.Helper()

	ctx, cancel := context.WithTimeout(env.goCtx, time.Minute)
	defer cancel()

	task, err := vm.PowerOff(ctx)
	require.NoError(t, err)
	require.NoError(t, task.Wait(ctx))
}

// containerID returns the ID of the container backing vm.
func containerID(t *testing.T, env *vcSimEnv, vm *VirtualMachine) string {
	t.Helper()

	var id string
	env.simCtx.WithLock(vm, func() {
		if vm.svm != nil && vm.svm.c != nil {
			id = vm.svm.c.id
		}
	})
	require.NotEmpty(t, id)

	return id
}

// dockerExec runs args in container id and returns stdout, trimmed, and the
// command's error. Stderr is kept out of stdout and returned in the error.
func dockerExec(ctx context.Context, id string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, "docker", append([]string{"exec", id}, args...)...).Output()
	if stderr := exitErrStderr(err); stderr != "" {
		err = fmt.Errorf("%w: %s", err, stderr)
	}
	return strings.TrimSpace(string(out)), err
}

// sleepForever keeps a container running and lets it stop promptly.
const sleepForever = "trap exit TERM; sleep infinity & wait"

// TestVMCI_ToolboxBinary_GuestInfoRoundTrip runs the toolbox binary as
// vmtoolsd and vmware-rpctool inside a container, as the bootstrapper and
// cloud-init do. TestToolboxCommand_OutputContract pins the exact output;
// here stderr also carries the docker CLI's own messages, so only stdout and
// the exit status are checked.
func TestVMCI_ToolboxBinary_GuestInfoRoundTrip(t *testing.T) {
	if !test.HasDocker() {
		t.Skip("requires docker or podman (aliased as docker) on linux")
	}

	env := newVCSIMEnv(t)
	vm, vmObj := powerOnContainerVM(t, env, "toolbox-roundtrip-test",
		&types.OptionValue{Key: ContainerBackingOptionKey, Value: `["alpine","sh","-c","` + sleepForever + `"]`},
		&types.OptionValue{Key: "RUN.vmci", Value: "true"},
		&types.OptionValue{Key: "RUN.volume.vmtoolsd", Value: toolboxBinary(t) + ":/usr/bin/vmtoolsd:ro"},
	)
	id := containerID(t, env, vmObj)

	const key = "guestinfo.toolbox-roundtrip"
	const val = "hello from toolbox"

	t.Run("vmtoolsd-info-set", func(t *testing.T) {
		out, err := dockerExec(env.goCtx, id, "vmtoolsd", "--cmd", "info-set "+key+" "+val)
		require.NoError(t, err)
		require.Empty(t, out)

		waitExtraConfig(t, env, vm.Reference(), key, val, time.Second)
	})

	t.Run("vmtoolsd-info-get", func(t *testing.T) {
		out, err := dockerExec(env.goCtx, id, "vmtoolsd", "--cmd", "info-get "+key)
		require.NoError(t, err)
		require.Equal(t, val, out)
	})

	t.Run("vmtoolsd-info-get-missing", func(t *testing.T) {
		out, err := dockerExec(env.goCtx, id, "vmtoolsd", "--cmd", "info-get guestinfo.nonexistent")

		var exitErr *exec.ExitError
		require.True(t, errors.As(err, &exitErr), "want a non-zero exit, got %v", err)
		require.Equal(t, 1, exitErr.ExitCode())
		require.Empty(t, out)
	})

	t.Run("vmware-rpctool-info-get", func(t *testing.T) {
		out, err := dockerExec(env.goCtx, id, "vmware-rpctool", "info-get "+key)
		require.NoError(t, err)
		require.Equal(t, val, out)
	})

	t.Run("vmware-rpctool-permission-denied", func(t *testing.T) {
		out, err := dockerExec(env.goCtx, id, "vmware-rpctool", "info-set RUN.container evil")

		var exitErr *exec.ExitError
		require.True(t, errors.As(err, &exitErr), "want a non-zero exit, got %v", err)
		require.Equal(t, 1, exitErr.ExitCode())
		require.Empty(t, out)
	})

	t.Run("power-cycle", func(t *testing.T) {
		powerOff(t, env, vm)
		powerOn(t, env, vm)

		out, err := dockerExec(env.goCtx, id, "vmtoolsd", "--cmd", "info-get "+key)
		require.NoError(t, err)
		require.Equal(t, val, out)
	})

	powerOff(t, env, vm)
}

// TestVMCI_SystemdInit_GuestInfoRoundTrip has a systemd unit, with none of the
// container's environment, write guestinfo through the well-known socket path.
func TestVMCI_SystemdInit_GuestInfoRoundTrip(t *testing.T) {
	testSystemdUnitWritesGuestInfo(t, "RUN.privileged")
}

// TestVMCI_NestedContainers_GuestInfoRoundTrip is
// TestVMCI_SystemdInit_GuestInfoRoundTrip under RUN.nestedContainers, whose
// tmpfs at /run must not hide the socket.
func TestVMCI_NestedContainers_GuestInfoRoundTrip(t *testing.T) {
	testSystemdUnitWritesGuestInfo(t, "RUN.nestedContainers")
}

func testSystemdUnitWritesGuestInfo(t *testing.T, option string) {
	if !test.HasDocker() {
		t.Skip("requires docker or podman (aliased as docker) on linux")
	}
	buildVMCISystemdInitImage(t)

	env := newVCSIMEnv(t)
	vm, _ := powerOnContainerVM(t, env, "systemd-"+strings.ToLower(strings.TrimPrefix(option, "RUN.")),
		&types.OptionValue{Key: ContainerBackingOptionKey, Value: `["` + vmciSystemdInitImage + `"]`},
		&types.OptionValue{Key: "RUN.vmci", Value: "true"},
		&types.OptionValue{Key: option, Value: "true"},
	)

	// systemd in a container typically reaches the unit in 2 to 30 seconds
	waitExtraConfig(t, env, vm.Reference(), "guestinfo.vmci-systemd-test", "pass", 2*time.Minute)

	powerOff(t, env, vm)
}

// TestContainerGuestRPC_MultiVM pins that concurrent container-backed VMs each
// reach only their own guestinfo.
func TestContainerGuestRPC_MultiVM(t *testing.T) {
	if !test.HasDocker() {
		t.Skip("requires docker or podman (aliased as docker) on linux")
	}

	env := newVCSIMEnv(t)

	var vms []*object.VirtualMachine
	var ids []string
	for i := range 2 {
		cmd := fmt.Sprintf("vmware-rpctool 'info-set guestinfo.multivm vm%d' && %s", i, sleepForever)
		vm, vmObj := powerOnContainerVM(t, env, fmt.Sprintf("guestrpc-multivm-%d", i),
			&types.OptionValue{Key: ContainerBackingOptionKey, Value: fmt.Sprintf(`["alpine","sh","-c",%q]`, cmd)},
			&types.OptionValue{Key: "RUN.vmci", Value: "true"},
		)
		vms = append(vms, vm)
		ids = append(ids, containerID(t, env, vmObj))
	}

	for i, vm := range vms {
		want := fmt.Sprintf("vm%d", i)
		waitExtraConfig(t, env, vm.Reference(), "guestinfo.multivm", want, 30*time.Second)

		out, err := dockerExec(env.goCtx, ids[i], "vmware-rpctool", "info-get guestinfo.multivm")
		require.NoError(t, err)
		require.Equal(t, want, out)
	}

	for _, vm := range vms {
		powerOff(t, env, vm)
	}
}
