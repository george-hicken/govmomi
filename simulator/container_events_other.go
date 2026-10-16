// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

//go:build !unix

package simulator

import (
	"os"
	"os/exec"
)

// startInOwnProcessGroup is a no-op where process groups are not available.
func startInOwnProcessGroup(*exec.Cmd) {}

// killProcessGroup kills p. Without process groups, children of p are not reached.
func killProcessGroup(p *os.Process) error {
	return p.Kill()
}
