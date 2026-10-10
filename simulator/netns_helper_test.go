// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package simulator

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
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
