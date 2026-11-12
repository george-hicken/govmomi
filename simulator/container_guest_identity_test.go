// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package simulator

import (
	"archive/tar"
	"bytes"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vmware/govmomi/test"
	"github.com/vmware/govmomi/vim25/types"
)

var guestIdentityAllFiles = guestIdentitySet{hosts: true, hostname: true, resolv: true}

func TestParseGuestIdentity(t *testing.T) {
	tests := []struct {
		value string
		want  guestIdentitySet
	}{
		{value: "owned", want: guestIdentityAllFiles},
		{value: "hosts", want: guestIdentitySet{hosts: true}},
		{value: "hostname", want: guestIdentitySet{hostname: true}},
		{value: "resolv", want: guestIdentitySet{resolv: true}},
		{value: "hosts,hostname", want: guestIdentitySet{hosts: true, hostname: true}},
		{value: "hostname,hosts", want: guestIdentitySet{hosts: true, hostname: true}},
		{value: "resolv,hostname,hosts", want: guestIdentityAllFiles},
	}
	for _, tc := range tests {
		t.Run(tc.value, func(t *testing.T) {
			got, err := parseGuestIdentity(tc.value)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}

	refused := []string{
		"", "true", "Owned", "owned ", "owned,hosts", "hosts,", ",hosts",
		"hosts,hosts", "hosts, hostname", "resolv.conf", "dns", "hosts;hostname",
	}
	for _, value := range refused {
		t.Run("refused "+value, func(t *testing.T) {
			got, err := parseGuestIdentity(value)
			assert.ErrorIs(t, err, errGuestIdentityPolicy)
			assert.Equal(t, guestIdentitySet{}, got)
		})
	}
}

// TestGuestIdentitySetFieldsAreNamed fails when a file is added to guestIdentitySet
// without a parser token for it, which would leave the file unreachable from RUN.
func TestGuestIdentitySetFieldsAreNamed(t *testing.T) {
	tokens := []string{"hosts", "hostname", "resolv"}
	typ := reflect.TypeOf(guestIdentitySet{})
	require.Equal(t, len(tokens), typ.NumField(), "one parser token per guestIdentitySet field")

	named := map[int]bool{}
	for _, token := range tokens {
		set, err := parseGuestIdentity(token)
		require.NoError(t, err)
		v := reflect.ValueOf(set)
		for i := range v.NumField() {
			if v.Field(i).Bool() {
				named[i] = true
			}
		}
	}
	for i := range typ.NumField() {
		assert.True(t, named[i], "no parser token owns %s", typ.Field(i).Name)
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

	// owned: the arguments the option has always produced.
	want := []string{"--no-hostname", "--no-hosts", "--dns", "none", "--hostname", "vm-1", "--cap-add", "SYS_ADMIN"}
	assert.Equal(t, want, guestIdentityCreateArgs("VM 1", createOptions{GuestIdentity: guestIdentityAllFiles}))

	// Each file adds only its own flags; SYS_ADMIN comes with an owned hostname alone.
	perFile := []struct {
		name  string
		set   guestIdentitySet
		flags []string
	}{
		{name: "hosts,hostname", set: guestIdentitySet{hosts: true, hostname: true},
			flags: []string{"--no-hostname", "--no-hosts", "--hostname", "vm-1", "--cap-add", "SYS_ADMIN"}},
		{name: "hosts", set: guestIdentitySet{hosts: true}, flags: []string{"--no-hosts"}},
		{name: "resolv", set: guestIdentitySet{resolv: true}, flags: []string{"--dns", "none"}},
		{name: "hostname", set: guestIdentitySet{hostname: true},
			flags: []string{"--no-hostname", "--hostname", "vm-1", "--cap-add", "SYS_ADMIN"}},
	}
	for _, tc := range perFile {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.flags, guestIdentityCreateArgs("VM 1", createOptions{GuestIdentity: tc.set}))
		})
	}

	// SYS_ADMIN is not duplicated when another option already grants it.
	granted := map[string]createOptions{
		"sysctl":     {GuestIdentity: guestIdentityAllFiles, Sysctl: true},
		"cap-add":    {GuestIdentity: guestIdentityAllFiles, CapAdd: []string{"NET_RAW", "SYS_ADMIN"}},
		"privileged": {GuestIdentity: guestIdentityAllFiles, Privileged: true},
		"nested":     {GuestIdentity: guestIdentityAllFiles, NestedContainers: true},
	}
	for name, opts := range granted {
		t.Run(name, func(t *testing.T) {
			args := guestIdentityCreateArgs("vm", opts)
			assert.NotContains(t, args, "SYS_ADMIN")
			assert.Contains(t, args, "--no-hostname")
		})
	}
}

func TestGuestIdentitySeeds(t *testing.T) {
	got := guestIdentitySeeds("My VM", guestIdentitySet{hosts: true, hostname: true})
	assert.Equal(t, []guestIdentitySeed{
		{path: "etc/hosts", content: "::1\tipv6-localhost ipv6-loopback\n127.0.0.1\tlocalhost\n127.0.0.1\tmy-vm\n"},
		{path: "etc/hostname", content: "my-vm\n"},
	}, got)

	// resolv has no seed: the runtime's resolv.conf is kept, or none is written under --dns none.
	assert.Empty(t, guestIdentitySeeds("vm", guestIdentitySet{resolv: true}))
}

func TestMissingGuestIdentitySeeds(t *testing.T) {
	seeds := guestIdentitySeeds("vm", guestIdentitySet{hosts: true, hostname: true})

	missing := missingGuestIdentitySeeds(seeds, map[string]bool{"etc/hosts": true})
	assert.Equal(t, []guestIdentitySeed{{path: "etc/hostname", content: "vm\n"}}, missing)

	assert.Empty(t, missingGuestIdentitySeeds(seeds, map[string]bool{"etc/hosts": true, "etc/hostname": true}))
}

func TestTarNames(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, h := range []*tar.Header{
		{Name: "etc/", Typeflag: tar.TypeDir, Mode: 0o755},
		{Name: "etc/hosts", Typeflag: tar.TypeReg, Mode: 0o644},
		{Name: "etc/ssl/", Typeflag: tar.TypeDir, Mode: 0o755},
	} {
		require.NoError(t, tw.WriteHeader(h))
	}
	require.NoError(t, tw.Close())

	names, err := tarNames(&buf)
	require.NoError(t, err)
	assert.Equal(t, map[string]bool{"etc": true, "etc/hosts": true, "etc/ssl": true}, names)
}

// guestIdentityMounts runs the script that reports, for each file the option
// covers, whether it is a mount point.
const guestIdentityMounts = `for f in /etc/hostname /etc/hosts /etc/resolv.conf; do ` +
	`awk -v f="$f" 'BEGIN{m="owned"} $5==f {m="mounted"} END{print f, m}' /proc/self/mountinfo; done`

// guestIdentityReplace replaces each file the way an atomic write does: a temporary
// file moved over the target. The move fails with EBUSY on a runtime bind mount.
func guestIdentityReplace(files ...string) string {
	var cmds []string
	for _, f := range files {
		cmds = append(cmds, "echo x > /tmp/r && mv /tmp/r "+f)
	}
	return strings.Join(append(cmds, "echo moved"), " && ")
}

func TestGuestIdentityFilesContainer(t *testing.T) {
	test.RequireContainerOptIn(t)
	if !test.HasDocker() {
		t.Skip("requires docker or podman (aliased as docker) on linux")
	}
	if !runtimeNetwork().podman {
		t.Skip("RUN.guestIdentityFiles needs podman as the docker CLI")
	}
	const vmName = "guest-identity-test"

	// start powers on the test container with the option value, or without the option when value is empty.
	start := func(t *testing.T, value string) (*vcSimEnv, string) {
		env := newVCSIMEnv(t)
		extra := []types.BaseOptionValue{
			&types.OptionValue{Key: ContainerBackingOptionKey, Value: `["busybox","sh","-c",` + strconv.Quote(sleepForever) + `]`},
		}
		if value != "" {
			extra = append(extra, &types.OptionValue{Key: "RUN.guestIdentityFiles", Value: value})
		}
		_, vm := powerOnContainerVM(t, env, vmName, extra...)
		return env, containerID(t, env, vm)
	}

	// exec runs the command in the container and returns its trimmed output.
	exec := func(t *testing.T, env *vcSimEnv, id string, args ...string) string {
		out, err := dockerExec(env.goCtx, id, args...)
		require.NoError(t, err)
		return strings.TrimSpace(out)
	}

	t.Run("owned", func(t *testing.T) {
		env, id := start(t, guestIdentityAll)

		assert.Equal(t, "/etc/hostname owned\n/etc/hosts owned\n/etc/resolv.conf owned",
			exec(t, env, id, "sh", "-c", guestIdentityMounts))
		assert.Equal(t, vmName, exec(t, env, id, "hostname"))
		assert.Equal(t, vmName, exec(t, env, id, "cat", "/etc/hostname"))
		assert.Contains(t, exec(t, env, id, "cat", "/etc/hosts"), "127.0.0.1\t"+vmName)

		assert.Equal(t, "moved", exec(t, env, id, "sh", "-c", guestIdentityReplace("/etc/hosts", "/etc/hostname", "/etc/resolv.conf")))
		assert.Equal(t, "renamed", exec(t, env, id, "sh", "-c", "hostname renamed && hostname"))
	})

	t.Run("hosts,hostname", func(t *testing.T) {
		env, id := start(t, "hosts,hostname")

		// resolv.conf stays podman's bind mount.
		assert.Equal(t, "/etc/hostname owned\n/etc/hosts owned\n/etc/resolv.conf mounted",
			exec(t, env, id, "sh", "-c", guestIdentityMounts))
		assert.Equal(t, vmName, exec(t, env, id, "hostname"))
		assert.Contains(t, exec(t, env, id, "cat", "/etc/hosts"), "127.0.0.1\t"+vmName)

		assert.Equal(t, "moved", exec(t, env, id, "sh", "-c", guestIdentityReplace("/etc/hosts", "/etc/hostname")))

		_, err := dockerExec(env.goCtx, id, "sh", "-c", guestIdentityReplace("/etc/resolv.conf"))
		assert.Error(t, err, "rename over the runtime-mounted /etc/resolv.conf should fail")
	})

	t.Run("default", func(t *testing.T) {
		env, id := start(t, "")

		assert.Equal(t, "/etc/hostname mounted\n/etc/hosts mounted\n/etc/resolv.conf mounted",
			exec(t, env, id, "sh", "-c", guestIdentityMounts))

		// Controls: without the option, rename over either runtime-mounted file fails.
		for _, f := range []string{"/etc/hosts", "/etc/hostname"} {
			_, err := dockerExec(env.goCtx, id, "sh", "-c", guestIdentityReplace(f))
			assert.Error(t, err, "rename over a runtime-mounted %s should fail", f)
		}
	})
}
