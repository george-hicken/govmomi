// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package simulator

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/vmware/vmw-guestinfo/vmcheck"

	"github.com/vmware/govmomi/toolbox"
)

// TestToolboxCommand_OutputContract pins the toolbox binary, run as vmtoolsd
// --cmd and as vmware-rpctool, to the output of the open-vm-tools commands it
// stands in for (services/vmtoolsd/cmdLine.c ToolsCoreRunCommand and
// rpctool/rpctool.c RpcToolCommand): the reply without its "1 " or "0 "
// prefix, on stdout with exit status 0 or on stderr with exit status 1.
// cloud-init and the Supervisor bootstrapper both parse that output.
//
// The binary runs on the host against the server RUN.vmci starts, so stdout
// and stderr are exactly the binary's.
func TestToolboxCommand_OutputContract(t *testing.T) {
	env := newVCSIMEnv(t)
	srv, _ := serveGuestRPC(t, env, allVMs(t, env)[0])

	bin := toolboxBinary(t)
	dir := t.TempDir()
	for _, name := range []string{"vmtoolsd", "vmware-rpctool"} {
		require.NoError(t, os.Symlink(bin, filepath.Join(dir, name)))
	}

	run := func(t *testing.T, socket, name string, args ...string) (string, string, int) {
		t.Helper()

		cmd := exec.Command(filepath.Join(dir, name), args...)
		cmd.Env = append(os.Environ(), toolbox.UnixSocketEnv+"="+socket)
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr

		err := cmd.Run()
		var exitErr *exec.ExitError
		if err != nil && !errors.As(err, &exitErr) {
			t.Fatal(err)
		}
		return stdout.String(), stderr.String(), cmd.ProcessState.ExitCode()
	}

	tests := []struct {
		name           string
		command        string
		args           []string
		stdout, stderr string
		exit           int
	}{
		{"vmtoolsd info-set", "vmtoolsd", []string{"--cmd", "info-set guestinfo.contract a value"}, "\n", "", 0},
		{"vmtoolsd info-get", "vmtoolsd", []string{"--cmd", "info-get guestinfo.contract"}, "a value\n", "", 0},
		{"vmtoolsd info-get missing", "vmtoolsd", []string{"--cmd", "info-get guestinfo.missing"}, "", "No value found\n", 1},
		{"vmtoolsd info-set refused", "vmtoolsd", []string{"--cmd", "info-set RUN.container evil"}, "", "Permission denied\n", 1},
		{"vmware-rpctool info-get", "vmware-rpctool", []string{"info-get guestinfo.contract"}, "a value\n", "", 0},
		{"vmware-rpctool info-get missing", "vmware-rpctool", []string{"info-get guestinfo.missing"}, "", "No value found\n", 1},
		{"vmware-rpctool info-set refused", "vmware-rpctool", []string{"info-set RUN.container evil"}, "", "Permission denied\n", 1},
		{"vmware-rpctool unknown command", "vmware-rpctool", []string{"no-such-command"}, "", "Unknown command\n", 1},
		{"vmware-rpctool usage", "vmware-rpctool", nil, "", "rpctool syntax:\n\n  -h | --help\tprint usage.\n  rpctool <text>\tsend <text> as an RPC command.\n", 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stdout, stderr, exit := run(t, srv.path, tt.command, tt.args...)
			require.Equal(t, tt.stdout, stdout, "stdout")
			require.Equal(t, tt.stderr, stderr, "stderr")
			require.Equal(t, tt.exit, exit, "exit status")
		})
	}

	t.Run("no transport", func(t *testing.T) {
		if _, err := os.Stat(toolbox.UnixSocketPath); err == nil {
			t.Skipf("%s exists on this host", toolbox.UnixSocketPath)
		}
		if vmcheck.IsVirtualCPU() {
			t.Skip("this host is a VMware VM, so the backdoor answers")
		}
		missing := filepath.Join(dir, "missing.sock")

		stdout, stderr, exit := run(t, missing, "vmtoolsd", "--cmd", "info-get guestinfo.contract")
		require.Equal(t, "", stdout)
		require.Equal(t, "Unable to send command to VMware hypervisor.\n", stderr)
		require.Equal(t, 1, exit)

		rpctool := filepath.Join(dir, "vmware-rpctool")
		stdout, stderr, exit = run(t, missing, "vmware-rpctool", "info-get guestinfo.contract")
		require.Equal(t, "", stdout)
		require.Equal(t, "Error: "+rpctool+" must be run inside a virtual machine on a VMware hypervisor product.\n", stderr)
		require.Equal(t, 255, exit)
	})
}
