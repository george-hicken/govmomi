// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package simulator

import (
	"os"
	"syscall"
)

// killProcessGroup kills the process group led by p, including children that stayed in it.
func killProcessGroup(p *os.Process) error {
	return syscall.Kill(-p.Pid, syscall.SIGKILL)
}
