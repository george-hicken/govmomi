// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package simulator

import (
	"bytes"
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/vmware/govmomi/guest"
	"github.com/vmware/govmomi/test"
	"github.com/vmware/govmomi/vim25/soap"
	"github.com/vmware/govmomi/vim25/types"
)

// TestContainerGuestFileTransfer uploads a file into a container-backed VM
// through the guest file manager, as wcpsvc's hypercrypt delivery does, and
// checks that the file reaches the container with the permissions and owner the
// request names, and that the same session cannot transfer once tools stop.
func TestContainerGuestFileTransfer(t *testing.T) {
	test.RequireContainerOptIn(t)
	if !test.HasDocker() {
		t.Skip("requires docker or podman (aliased as docker) on linux")
	}

	env := newVCSIMEnv(t)
	vmObj, vm := powerOnContainerVM(t, env, "guest-file-transfer-test",
		&types.OptionValue{Key: ContainerBackingOptionKey, Value: `["alpine","sh","-c","` + sleepForever + `"]`},
	)
	requireToolsRunningStatus(t, env, vm, guestToolsRunning)

	ctx := context.Background()
	ops := guest.NewOperationsManager(env.client.Client, vmObj.Reference())
	fm, err := ops.FileManager(ctx)
	require.NoError(t, err)
	auth := &types.NamePasswordAuthentication{Username: "root", Password: "pw"}

	upload := func(path string, attr types.BaseGuestFileAttributes, content []byte) error {
		u, err := fm.InitiateFileTransferToGuest(ctx, auth, path, attr, int64(len(content)), true)
		if err != nil {
			return err
		}
		dst, err := fm.TransferURL(ctx, u)
		if err != nil {
			return err
		}
		return env.client.Client.Upload(ctx, bytes.NewReader(content), dst, &soap.Upload{Method: "PUT", ContentLength: int64(len(content))})
	}

	t.Run("applies the requested permissions and owner", func(t *testing.T) {
		uid, gid := int32(1000), int32(1001)
		require.NoError(t, upload("/dev/shm/secret.tmp",
			&types.GuestPosixFileAttributes{Permissions: 0400, OwnerId: &uid, GroupId: &gid}, []byte("c2VjcmV0\n")))

		stat, err := dockerExec(ctx, vm.svm.c.id, "stat", "-c", "%a %u:%g", "/dev/shm/secret.tmp")
		require.NoError(t, err)
		require.Equal(t, "400 1000:1001", stat)
		out, err := dockerExec(ctx, vm.svm.c.id, "cat", "/dev/shm/secret.tmp")
		require.NoError(t, err)
		require.Equal(t, "c2VjcmV0", out)
	})

	t.Run("keeps the default mode when none is requested", func(t *testing.T) {
		require.NoError(t, upload("/tmp/plain", nil, []byte("x")))
		stat, err := dockerExec(ctx, vm.svm.c.id, "stat", "-c", "%a %u:%g", "/tmp/plain")
		require.NoError(t, err)
		require.Equal(t, "444 0:0", stat)
	})

	t.Run("is refused while tools are not running", func(t *testing.T) {
		env.simCtx.WithLock(vm, func() {
			vm.Guest.ToolsRunningStatus = string(types.VirtualMachineToolsRunningStatusGuestToolsNotRunning)
		})
		err := upload("/tmp/refused", nil, []byte("x"))
		require.True(t, soap.IsSoapFault(err), "got %v", err)
		require.IsType(t, types.GuestOperationsUnavailable{}, soap.ToSoapFault(err).VimFault())
	})
}
