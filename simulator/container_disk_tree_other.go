// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

//go:build !linux

package simulator

import "errors"

func mountTree(pid int, src, dest string) error {
	return errors.New("presenting a disk to a container VM needs linux")
}
