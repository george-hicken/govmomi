// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

//go:build unix && !linux

package simulator

import (
	"os/exec"
	"syscall"
)

// startInOwnProcessGroup makes cmd the leader of a new process group, so killProcessGroup reaches the children docker's
// CLI forks. Residual: no Pdeathsig here, so a hard-killed vcsim leaves docker events running.
func startInOwnProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}
