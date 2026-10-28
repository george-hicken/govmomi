// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package simulator

import (
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/test"
	"github.com/vmware/govmomi/vim25/types"
)

func TestParseVirt(t *testing.T) {
	v, err := parseVirt("vmware")
	require.NoError(t, err)
	assert.True(t, v)

	v, err = parseVirt("container")
	require.NoError(t, err)
	assert.False(t, v)

	for _, value := range []string{"", "VMware", "true", "podman"} {
		_, err := parseVirt(value)
		assert.ErrorIs(t, err, errVirtPolicy, value)
	}
}

// TestDetectVirtScript pins the answers systemd-detect-virt gives in the
// guest of a VMware VM, for the flags cloud-init, ds-identify and systemd's
// own tools use.
func TestDetectVirtScript(t *testing.T) {
	file, err := detectVirtShim()
	require.NoError(t, err)

	again, err := detectVirtShim()
	require.NoError(t, err)
	assert.Equal(t, file, again)

	info, err := os.Stat(file)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0755), info.Mode().Perm())

	tests := []struct {
		args []string
		out  string
		code int
	}{
		{nil, "vmware", 0},
		{[]string{"--vm"}, "vmware", 0},
		{[]string{"-v"}, "vmware", 0},
		{[]string{"--quiet"}, "", 0},
		{[]string{"--quiet", "--vm"}, "", 0},
		{[]string{"--container"}, "none", 1},
		{[]string{"-c"}, "none", 1},
		{[]string{"--quiet", "--container"}, "", 1},
		{[]string{"--cvm"}, "none", 1},
		{[]string{"--chroot"}, "", 1},
		{[]string{"--private-users"}, "", 1},
		{[]string{"--list"}, "vmware", 0},
	}
	for _, tc := range tests {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			out, err := exec.Command(file, tc.args...).Output()
			code := 0
			var exit *exec.ExitError
			if errors.As(err, &exit) {
				code = exit.ExitCode()
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tc.code, code)
			assert.Equal(t, tc.out, strings.TrimSpace(string(out)))
		})
	}
}

func TestVirtCreateArgs(t *testing.T) {
	assert.Nil(t, virtCreateArgs(createOptions{}))
	assert.Nil(t, virtCreateArgs(createOptions{VMwareVirt: true, Env: []string{"container=lxc"}}))

	want := []string{"--unsetenv", "container"}
	if !runtimeNetwork().podman {
		want = nil
	}
	assert.Equal(t, want, virtCreateArgs(createOptions{VMwareVirt: true, Env: []string{"A=b"}}))
}

func TestVirtMounts(t *testing.T) {
	podman := runtimeNetwork().podman
	mounts, err := virtMounts(nil)
	require.NoError(t, err)
	if podman {
		require.Len(t, mounts, 2)
		assert.True(t, strings.HasSuffix(mounts[1], ":"+containerMarkerPath+":ro"), mounts[1])
		file, _, _ := strings.Cut(mounts[1], ":")
		content, err := os.ReadFile(file)
		require.NoError(t, err)
		assert.Equal(t, "podman", string(content))
	} else {
		require.Len(t, mounts, 1)
	}
	assert.True(t, strings.HasSuffix(mounts[0], ":"+detectVirtPath+":ro"), mounts[0])

	mounts, err = virtMounts([]types.BaseOptionValue{
		&types.OptionValue{Key: "RUN.volume.virt", Value: "/bin/true:" + detectVirtPath + ":ro"},
		&types.OptionValue{Key: "RUN.volume.ctr", Value: "/etc/hostname:" + containerMarkerPath + ":ro"},
	})
	require.NoError(t, err)
	assert.Empty(t, mounts)

	// A user's container= is kept, so systemd writes the marker itself.
	mounts, err = virtMounts([]types.BaseOptionValue{
		&types.OptionValue{Key: "RUN.env.container", Value: "lxc"},
	})
	require.NoError(t, err)
	require.Len(t, mounts, 1)
	assert.True(t, strings.HasSuffix(mounts[0], ":"+detectVirtPath+":ro"), mounts[0])
}

// pid1Container prints PID 1's container= variable, or nothing.
const pid1Container = `tr '\0' '\n' </proc/1/environ | grep '^container=' || true`

func TestVirtContainer(t *testing.T) {
	if !test.HasDocker() {
		t.Skip("requires docker or podman (aliased as docker) on linux")
	}
	busybox := &types.OptionValue{Key: ContainerBackingOptionKey, Value: `["busybox","sh","-c",` + strconv.Quote(sleepForever) + `]`}

	t.Run("vmware", func(t *testing.T) {
		env := newVCSIMEnv(t)
		vm, simVM := powerOnContainerVM(t, env, "virt-vmware-test", busybox)
		id := containerID(t, env, simVM)

		check := func() {
			t.Helper()
			out, err := dockerExec(env.goCtx, id, "systemd-detect-virt")
			require.NoError(t, err)
			assert.Equal(t, "vmware", out)

			_, err = dockerExec(env.goCtx, id, "systemd-detect-virt", "--quiet", "--container")
			assert.Error(t, err, "the guest must not detect a container")

			out, err = dockerExec(env.goCtx, id, "sh", "-c", pid1Container)
			require.NoError(t, err)
			assert.Empty(t, out, "PID 1 must have no container= variable")

			if runtimeNetwork().podman {
				out, err = dockerExec(env.goCtx, id, "cat", containerMarkerPath)
				require.NoError(t, err)
				assert.Equal(t, "podman", out, "the container marker systemd would write")
			}
		}
		check()

		// A later start of the same container finds the shim.
		powerOff(t, env, vm)
		powerOn(t, env, vm)
		require.Equal(t, id, containerID(t, env, simVM))
		check()
	})

	t.Run("container", func(t *testing.T) {
		env := newVCSIMEnv(t)
		_, simVM := powerOnContainerVM(t, env, "virt-container-test", busybox,
			&types.OptionValue{Key: "RUN.virt", Value: "container"})
		id := containerID(t, env, simVM)

		_, err := dockerExec(env.goCtx, id, "test", "-e", detectVirtPath)
		assert.Error(t, err, "no shim with RUN.virt=container")
		_, err = dockerExec(env.goCtx, id, "test", "-e", containerMarkerPath)
		assert.Error(t, err, "no container marker mount with RUN.virt=container")

		if runtimeNetwork().podman {
			out, err := dockerExec(env.goCtx, id, "sh", "-c", pid1Container)
			require.NoError(t, err)
			assert.Equal(t, "container=podman", out)
		}
	})

	t.Run("invalid", func(t *testing.T) {
		env := newVCSIMEnv(t)
		spec := types.VirtualMachineConfigSpec{
			Name:  "virt-invalid-test",
			Files: &types.VirtualMachineFileInfo{VmPathName: "[LocalDS_0] virt-invalid-test"},
			ExtraConfig: []types.BaseOptionValue{busybox,
				&types.OptionValue{Key: "RUN.virt", Value: "kvm"}},
		}
		require.NoError(t, test.ApplyContainerRuntimeDefaults(&spec))
		task, err := env.folder.VmFolder.CreateVM(env.goCtx, spec, env.pool, nil)
		require.NoError(t, err)
		info, err := task.WaitForResult(env.goCtx, nil)
		require.NoError(t, err)

		vm := object.NewVirtualMachine(env.client.Client, info.Result.(types.ManagedObjectReference))
		task, err = vm.PowerOn(env.goCtx)
		require.NoError(t, err)
		_, err = task.WaitForResult(env.goCtx, nil)
		require.Error(t, err)
		// The power-on fault carries the reason in its cause.
		var taskErr interface{ Fault() types.BaseMethodFault }
		require.ErrorAs(t, err, &taskErr)
		fault, ok := taskErr.Fault().(*types.MissingPowerOnConfiguration)
		require.True(t, ok, "%T", taskErr.Fault())
		require.NotNil(t, fault.FaultCause)
		assert.Contains(t, fault.FaultCause.LocalizedMessage, "RUN.virt")
	})
}

// TestVirtSystemdGuest pins the guest a systemd container-backed VM sees with
// the RUN.virt defaults, so that a change to RUN.virt cannot silently change
// it: it detects a VMware VM and no container, PID 1 has no container=
// variable, /run/systemd/container still names the runtime, systemd boots,
// and DMI carries the VM's BIOS UUID. vcsim provides no DMI product_name.
// It uses vmciSystemdInitImage, a debian image with systemd as PID 1.
func TestVirtSystemdGuest(t *testing.T) {
	if !test.HasDocker() {
		t.Skip("requires docker or podman (aliased as docker) on linux")
	}
	buildVMCISystemdInitImage(t)

	env := newVCSIMEnv(t)
	_, simVM := powerOnContainerVM(t, env, "virt-systemd-test",
		&types.OptionValue{Key: ContainerBackingOptionKey, Value: `["` + vmciSystemdInitImage + `"]`})
	id := containerID(t, env, simVM)

	// systemd in a container typically finishes booting in 2 to 30 seconds.
	var state string
	require.Eventually(t, func() bool {
		state, _ = dockerExec(env.goCtx, id, "systemctl", "is-system-running")
		return state == "running" || state == "degraded"
	}, 2*time.Minute, time.Second, "systemctl is-system-running")
	assert.NotEqual(t, "offline", state)

	out, err := dockerExec(env.goCtx, id, "systemd-detect-virt")
	require.NoError(t, err)
	assert.Equal(t, "vmware", out)

	out, _ = dockerExec(env.goCtx, id, "systemd-detect-virt", "--container")
	assert.Equal(t, "none", out)

	out, err = dockerExec(env.goCtx, id, "sh", "-c", pid1Container)
	require.NoError(t, err)
	assert.Empty(t, out, "PID 1 must have no container= variable")

	want := "docker"
	if runtimeNetwork().podman {
		want = "podman"
	}
	out, err = dockerExec(env.goCtx, id, "cat", containerMarkerPath)
	require.NoError(t, err)
	assert.Equal(t, want, out)

	out, err = dockerExec(env.goCtx, id, "cat", "/sys/class/dmi/id/product_uuid")
	require.NoError(t, err)
	assert.Equal(t, productUUID(simVM.svm.biosUUID()), out)
}
