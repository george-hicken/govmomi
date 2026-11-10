// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package simulator

import (
	"bytes"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestNetnsHelperInit pins that a test binary with no TestMain serves as a netns helper when the runtime
// re-executes it: init runs first and exits, so the tests never start. The socket's directory does not exist,
// so the helper fails to listen and exits 1 with its error, rather than serving.
func TestNetnsHelperInit(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "missing", "helper.sock")
	cmd := exec.Command(os.Args[0], "-test.run=^TestNetnsHelperInit$")
	cmd.Env = append(os.Environ(), netnsHelperEnv+"="+sock)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	err := cmd.Run()
	var exitErr *exec.ExitError
	require.True(t, errors.As(err, &exitErr), "want the helper to exit with an error, got %v", err)
	require.Equal(t, 1, exitErr.ExitCode())
	require.Contains(t, stderr.String(), "netns helper:")
}

// TestNetnsResponseKeepsErrorKind: a failure crossing the helper socket comes back as the error a
// direct op returns, so callers that classify by errno or timeout see the same kind.
func TestNetnsResponseKeepsErrorKind(t *testing.T) {
	for _, errno := range []syscall.Errno{syscall.ECONNREFUSED, syscall.EHOSTUNREACH} {
		var resp netnsResponse
		resp.setErr(&net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", errno)})
		err := resp.err("dial", "10.0.2.99:6443")
		require.ErrorIs(t, err, errno)
		var oe *net.OpError
		require.ErrorAs(t, err, &oe)
		require.Equal(t, "dial", oe.Op)
	}

	var resp netnsResponse
	resp.setErr(&net.OpError{Op: "dial", Net: "tcp", Err: netnsTimeout("i/o timeout")})
	var ne net.Error
	require.ErrorAs(t, resp.err("dial", "10.0.2.99:6443"), &ne)
	require.True(t, ne.Timeout())

	require.NoError(t, netnsResponse{}.err("dial", "10.0.2.99:6443"))
	require.EqualError(t, netnsResponse{Error: "unknown op x"}.err("x", ""), "unknown op x")
}
