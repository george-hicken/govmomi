// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

//go:build !linux

package simulator

import "errors"

// buildToolbox fails: the toolbox binary is only built on linux hosts.
func buildToolbox() (string, error) {
	return "", errors.New("toolbox build: only supported on linux")
}

// VmciToolboxBinaryPath returns "": the toolbox binary is only built on linux hosts.
func VmciToolboxBinaryPath() string {
	return ""
}
