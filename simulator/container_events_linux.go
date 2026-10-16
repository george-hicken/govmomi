// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package simulator

import (
	"os/exec"
	"syscall"
)

// startInOwnProcessGroup makes cmd the leader of a new process group, so killProcessGroup reaches the children docker's
// CLI forks. Pdeathsig kills the direct child if vcsim dies hard. It is tied to the OS thread that forked the child, so
// a locked thread's exit could also trigger it; watch is not called from such a goroutine. Residual: children the
// docker CLI forks are not covered by Pdeathsig, so they still leak on a hard kill.
func startInOwnProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
}
