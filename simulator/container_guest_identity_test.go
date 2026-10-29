// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package simulator

import (
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vmware/govmomi/test"
	"github.com/vmware/govmomi/vim25/types"
)

func TestParseGuestIdentity(t *testing.T) {
	require.NoError(t, parseGuestIdentity("owned"))
	for _, value := range []string{"", "true", "Owned", "owned "} {
		assert.ErrorIs(t, parseGuestIdentity(value), errGuestIdentityPolicy, value)
	}
}

func TestGuestHostname(t *testing.T) {
	tests := []struct {
		name string
		want string
	}{
		{name: "guest-identity-test", want: "guest-identity-test"},
		{name: "My VM_1", want: "my-vm-1"},
		{name: "--a..b--", want: "a-b"},
		{name: "Ünïcode", want: "n-code"},
		{name: "___", want: "vm"},
		{name: "", want: "vm"},
		{name: strings.Repeat("a", 70), want: strings.Repeat("a", 63)},
		{name: strings.Repeat("a", 62) + "-b", want: strings.Repeat("a", 62)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := guestHostname(tc.name)
			assert.Equal(t, tc.want, got)
			assert.LessOrEqual(t, len(got), maxHostnameLength)
		})
	}
}

func TestGuestIdentityPreflight(t *testing.T) {
	assert.ErrorIs(t, guestIdentityPreflight([]string{"podman", "host"}), errGuestIdentityHostNetwork)
}

func TestGuestIdentityCreateArgs(t *testing.T) {
	// Absent key: no flags, so create's arguments are unchanged.
	assert.Nil(t, guestIdentityCreateArgs("vm", createOptions{Privileged: true}))
	assert.Nil(t, guestIdentityCreateArgs("vm", createOptions{}))

	want := []string{"--no-hostname", "--no-hosts", "--dns", "none", "--hostname", "vm-1", "--cap-add", "SYS_ADMIN"}
	assert.Equal(t, want, guestIdentityCreateArgs("VM 1", createOptions{GuestIdentityFiles: true}))

	// SYS_ADMIN is not duplicated when another option already grants it.
	granted := map[string]createOptions{
		"sysctl":     {GuestIdentityFiles: true, Sysctl: true},
		"cap-add":    {GuestIdentityFiles: true, CapAdd: []string{"NET_RAW", "SYS_ADMIN"}},
		"privileged": {GuestIdentityFiles: true, Privileged: true},
		"nested":     {GuestIdentityFiles: true, NestedContainers: true},
	}
	for name, opts := range granted {
		t.Run(name, func(t *testing.T) {
			args := guestIdentityCreateArgs("vm", opts)
			assert.NotContains(t, args, "SYS_ADMIN")
			assert.Contains(t, args, "--no-hostname")
		})
	}
}

// guestIdentityMounts runs the script that reports, for each file the option
// covers, whether it is a mount point.
const guestIdentityMounts = `for f in /etc/hostname /etc/hosts /etc/resolv.conf; do ` +
	`awk -v f="$f" 'BEGIN{m="owned"} $5==f {m="mounted"} END{print f, m}' /proc/self/mountinfo; done`

func TestGuestIdentityFilesContainer(t *testing.T) {
	test.RequireContainerOptIn(t)
	if !test.HasDocker() {
		t.Skip("requires docker or podman (aliased as docker) on linux")
	}
	if !runtimeNetwork().podman {
		t.Skip("RUN.guestIdentityFiles needs podman as the docker CLI")
	}
	const vmName = "guest-identity-test"

	t.Run("owned", func(t *testing.T) {
		env := newVCSIMEnv(t)
		_, vm := powerOnContainerVM(t, env, vmName,
			&types.OptionValue{Key: ContainerBackingOptionKey, Value: `["busybox","sh","-c",` + strconv.Quote(sleepForever) + `]`},
			&types.OptionValue{Key: "RUN.guestIdentityFiles", Value: guestIdentityOwned},
		)
		id := containerID(t, env, vm)

		out, err := dockerExec(env.goCtx, id, "sh", "-c", guestIdentityMounts)
		require.NoError(t, err)
		assert.Equal(t, "/etc/hostname owned\n/etc/hosts owned\n/etc/resolv.conf owned", strings.TrimSpace(out))

		out, err = dockerExec(env.goCtx, id, "hostname")
		require.NoError(t, err)
		assert.Equal(t, vmName, strings.TrimSpace(out))

		// An atomic replace is what the runtime's bind mounts refuse with EBUSY.
		out, err = dockerExec(env.goCtx, id, "sh", "-c",
			"echo x > /tmp/h && mv /tmp/h /etc/hosts && echo y > /tmp/r && mv /tmp/r /etc/resolv.conf && echo moved")
		require.NoError(t, err)
		assert.Equal(t, "moved", strings.TrimSpace(out))

		out, err = dockerExec(env.goCtx, id, "sh", "-c", "hostname renamed && hostname")
		require.NoError(t, err)
		assert.Equal(t, "renamed", strings.TrimSpace(out))
	})

	t.Run("default", func(t *testing.T) {
		env := newVCSIMEnv(t)
		_, vm := powerOnContainerVM(t, env, vmName,
			&types.OptionValue{Key: ContainerBackingOptionKey, Value: `["busybox","sh","-c",` + strconv.Quote(sleepForever) + `]`},
		)
		id := containerID(t, env, vm)

		out, err := dockerExec(env.goCtx, id, "sh", "-c", guestIdentityMounts)
		require.NoError(t, err)
		assert.Equal(t, "/etc/hostname mounted\n/etc/hosts mounted\n/etc/resolv.conf mounted", strings.TrimSpace(out))

		_, err = dockerExec(env.goCtx, id, "sh", "-c", "echo x > /tmp/h && mv /tmp/h /etc/hosts")
		assert.Error(t, err, "rename over a runtime-mounted /etc/hosts should fail")
	})
}
