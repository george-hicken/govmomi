// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package simulator

import (
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/vmware/govmomi/vim25/types"
)

func TestGuestFileAttributes(t *testing.T) {
	uid, gid := int32(0), int32(1000)

	t.Run("round trip through the transfer URL", func(t *testing.T) {
		attr := posixFileAttributes(&types.GuestPosixFileAttributes{Permissions: 0400, OwnerId: &uid, GroupId: &gid})
		query := url.Values{}
		attr.addTo(query)

		got := guestFileAttributesFrom(query)
		require.Equal(t, int64(0400), got.mode)
		require.Equal(t, &uid, got.uid)
		require.Equal(t, &gid, got.gid)

		h := got.tarHeader("secret.tmp", 12)
		require.Equal(t, "secret.tmp", h.Name)
		require.Equal(t, int64(12), h.Size)
		require.Equal(t, int64(0400), h.Mode)
		require.Equal(t, 0, h.Uid)
		require.Equal(t, 1000, h.Gid)
	})

	t.Run("no attributes keep the default mode and owner", func(t *testing.T) {
		for _, attr := range []types.BaseGuestFileAttributes{nil, &types.GuestFileAttributes{}} {
			query := url.Values{}
			posixFileAttributes(attr).addTo(query)
			require.Empty(t, query)

			h := guestFileAttributesFrom(query).tarHeader("f", 1)
			require.Equal(t, int64(defaultGuestFileMode), h.Mode)
			require.Zero(t, h.Uid)
			require.Zero(t, h.Gid)
		}
	})

	t.Run("a malformed value names nothing", func(t *testing.T) {
		got := guestFileAttributesFrom(url.Values{"mode": {"rwx"}, "uid": {"-"}, "gid": {""}})
		require.Equal(t, guestFileAttributes{}, got)
	})
}

func TestPrepareGuestOperationRequiresRunningTools(t *testing.T) {
	auth := &types.NamePasswordAuthentication{Username: "root", Password: "pw"}
	newSVM := func(power types.VirtualMachinePowerState, tools string) *simVM {
		vm := &VirtualMachine{}
		vm.Runtime.PowerState = power
		vm.Guest = &types.GuestInfo{ToolsRunningStatus: tools}
		return &simVM{vm: vm, c: &container{id: "c1"}}
	}
	running := string(types.VirtualMachineToolsRunningStatusGuestToolsRunning)
	notRunning := string(types.VirtualMachineToolsRunningStatusGuestToolsNotRunning)

	require.Nil(t, newSVM(types.VirtualMachinePowerStatePoweredOn, running).prepareGuestOperation(auth))
	require.IsType(t, new(types.GuestOperationsUnavailable),
		newSVM(types.VirtualMachinePowerStatePoweredOn, notRunning).prepareGuestOperation(auth))
	require.IsType(t, new(types.InvalidPowerState),
		newSVM(types.VirtualMachinePowerStatePoweredOff, running).prepareGuestOperation(auth))
}
