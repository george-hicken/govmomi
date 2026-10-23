// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package simulator

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vmware/govmomi/test"
	"github.com/vmware/govmomi/vim25/types"
)

func TestParseSysctl(t *testing.T) {
	tests := []struct {
		value   string
		want    []string
		wantErr bool
	}{
		{value: "emulate=net.ipv4.tcp_mem,vm.overcommit_memory", want: []string{"net.ipv4.tcp_mem", "vm.overcommit_memory"}},
		{value: " emulate=kernel.panic , kernel.panic ", want: []string{"kernel.panic"}},
		{value: "emulate=", want: nil},
		{value: "net.ipv4.tcp_mem", wantErr: true},
		{value: "emulate=net.ipv4.tcp_mem/x", wantErr: true},
		{value: "emulate=net..ipv4", wantErr: true},
		{value: "emulate=Net.ipv4.tcp_mem", wantErr: true},
		{value: "emulate=net.ipv4.", wantErr: true},
		{value: "emulate=net.ipv4.tcp_mem,", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.value, func(t *testing.T) {
			p, err := parseSysctl(tc.value)
			if tc.wantErr {
				assert.ErrorIs(t, err, errSysctlPolicy)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, p.emulated)
		})
	}
}

func TestSysctlPreflight(t *testing.T) {
	assert.NoError(t, sysctlPreflight(nil, true))
	assert.NoError(t, sysctlPreflight([]string{"podman"}, true))
	assert.ErrorIs(t, sysctlPreflight([]string{"podman", "host"}, true), errSysctlHostNetwork)
	assert.ErrorIs(t, sysctlPreflight(nil, false), errSysctlPolicy)
}

func TestSysctlSeedValues(t *testing.T) {
	root := t.TempDir()
	write := func(key, value string) {
		p := filepath.Join(root, strings.ReplaceAll(key, ".", "/"))
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.WriteFile(p, []byte(value), 0o644))
	}
	write("net.ipv4.tcp_mem", "188436\t251249\t376872\n")
	write("vm.overcommit_memory", "0\n")

	seeds, err := sysctlSeedValues(root, []string{"net.ipv4.tcp_mem", "vm.overcommit_memory", "net.ipv4.absent"})
	require.NoError(t, err)
	assert.Equal(t, []sysctlValue{
		{Key: "net.ipv4.tcp_mem", Value: "188436\t251249\t376872"},
		{Key: "vm.overcommit_memory", Value: "0"},
		{Key: "net.ipv4.absent", Value: ""},
	}, seeds)

	write("kernel.multi", "a\nb\n")
	_, err = sysctlSeedValues(root, []string{"kernel.multi"})
	assert.ErrorIs(t, err, errSysctlPolicy)
}

func TestWriteSysctlOverlay(t *testing.T) {
	dir := t.TempDir()
	seeds := []sysctlValue{{Key: "net.ipv4.udp_mem", Value: "1 2 3"}, {Key: "kernel.panic", Value: "0"}}
	require.NoError(t, writeSysctlOverlay(dir, true, seeds))

	seed, err := os.ReadFile(filepath.Join(dir, sysctlSeedFileName))
	require.NoError(t, err)
	assert.Equal(t, "drop-sys-admin=true\nnet.ipv4.udp_mem=1 2 3\nkernel.panic=0\n", string(seed))

	script := filepath.Join(dir, "overlay.sh")
	info, err := os.Stat(script)
	require.NoError(t, err)
	assert.NotZero(t, info.Mode().Perm()&0o111, "overlay.sh is not executable")

	out, err := exec.Command("sh", "-n", script).CombinedOutput()
	require.NoError(t, err, string(out))

	dir = t.TempDir()
	require.NoError(t, writeSysctlOverlay(dir, false, nil))
	seed, err = os.ReadFile(filepath.Join(dir, sysctlSeedFileName))
	require.NoError(t, err)
	assert.Equal(t, "drop-sys-admin=false\n", string(seed))
}

func TestSysctlCreateArgs(t *testing.T) {
	args := sysctlCreateArgs()
	assert.Contains(t, args, "unmask=/proc/sys")
	assert.Contains(t, args, "SYS_ADMIN")
	assert.Contains(t, args, "NET_ADMIN")
	assert.Equal(t, "--entrypoint", args[len(args)-2])
	assert.Equal(t, sysctlOverlayEntrypoint, args[len(args)-1])
}

// hostSysctl reads a dotted host sysctl, as the test's own user.
func hostSysctl(t *testing.T, key string) string {
	t.Helper()
	b, err := os.ReadFile(sysctlKeyPath(key))
	require.NoError(t, err)
	return strings.TrimSpace(string(b))
}

// TestSysctlOverlayContainer runs busybox with RUN.sysctl and checks that namespaced
// writes are real, emulated writes read back, undeclared host-global writes fail, and the
// host's values do not change. It needs docker or podman.
func TestSysctlOverlayContainer(t *testing.T) {
	if !test.HasDocker() {
		t.Skip("requires docker or podman (aliased as docker) on linux")
	}

	emulated := []string{"net.ipv4.tcp_mem", "net.ipv4.udp_mem", "vm.overcommit_memory", "kernel.panic"}
	namespaced := "net.ipv4.conf.all.arp_ignore"
	const undeclared = "kernel.panic_on_oops"
	keys := append(slices.Clone(emulated), namespaced, undeclared)

	hostBefore := make(map[string]string)
	for _, key := range keys {
		hostBefore[key] = hostSysctl(t, key)
	}

	env := newVCSIMEnv(t)
	_, vm := powerOnContainerVM(t, env, "sysctl-overlay-test",
		&types.OptionValue{Key: ContainerBackingOptionKey, Value: `["busybox","sh","-c",` + strconv.Quote(sleepForever) + `]`},
		&types.OptionValue{Key: "RUN.sysctl", Value: "emulate=" + strings.Join(emulated, ",")},
	)
	id := containerID(t, env, vm)

	read := func(key string) string {
		t.Helper()
		out, err := dockerExec(env.goCtx, id, "cat", sysctlKeyPath(key))
		require.NoError(t, err, key)
		return out
	}
	write := func(key, value string) error {
		_, err := dockerExec(env.goCtx, id, "sh", "-c", "echo '"+value+"' > "+sysctlKeyPath(key))
		return err
	}

	t.Run("namespaced write is real", func(t *testing.T) {
		require.NoError(t, write(namespaced, "1"))
		assert.Equal(t, "1", read(namespaced))
	})

	t.Run("emulated writes read back", func(t *testing.T) {
		require.NoError(t, write("net.ipv4.tcp_mem", "4096 8192 16384"))
		assert.Equal(t, "4096 8192 16384", read("net.ipv4.tcp_mem"))
		require.NoError(t, write("vm.overcommit_memory", "2"))
		assert.Equal(t, "2", read("vm.overcommit_memory"))
	})

	t.Run("undeclared host-global write fails", func(t *testing.T) {
		assert.Error(t, write(undeclared, "1"))
	})

	t.Run("workload drops CAP_SYS_ADMIN where setpriv can", func(t *testing.T) {
		// busybox's setpriv has no --bounding-set, so the overlay keeps the capability there.
		_, err := dockerExec(env.goCtx, id, "sh", "-c", "setpriv --bounding-set -sys_admin true")
		if err != nil {
			t.Skip("setpriv in the image cannot drop a bounding capability")
		}
		out, err := dockerExec(env.goCtx, id, "grep", "CapBnd", "/proc/1/status")
		require.NoError(t, err)
		fields := strings.Fields(out)
		require.Len(t, fields, 2)
		bnd, err := strconv.ParseUint(fields[1], 16, 64)
		require.NoError(t, err)
		const capSysAdmin = 21
		assert.Zero(t, bnd&(1<<capSysAdmin), "CAP_SYS_ADMIN still in the workload's bounding set")
	})

	t.Run("evidence", func(t *testing.T) {
		evidence, err := vm.EmulatedSysctls(env.simCtx)
		require.NoError(t, err)
		require.Len(t, evidence, len(emulated))
		for _, e := range evidence {
			assert.Equal(t, SysctlEmulatedNotApplied, e.Status)
			assert.Equal(t, hostBefore[e.Key], e.Seed, e.Key)
		}
		assert.Equal(t, "4096 8192 16384", evidence[0].Current)
	})

	t.Run("host unchanged", func(t *testing.T) {
		for _, key := range keys {
			assert.Equal(t, hostBefore[key], hostSysctl(t, key), key)
		}
	})
}
