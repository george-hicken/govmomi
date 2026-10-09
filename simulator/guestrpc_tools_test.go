// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package simulator

import (
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/vmware/govmomi/toolbox"
	"github.com/vmware/govmomi/vim25/types"
)

var (
	guestToolsRunning    = string(types.VirtualMachineToolsRunningStatusGuestToolsRunning)
	guestToolsNotRunning = string(types.VirtualMachineToolsRunningStatusGuestToolsNotRunning)
)

// requireToolsRunningStatus waits for vm's guest and summary.guest to report
// want.
func requireToolsRunningStatus(t *testing.T, env *vcSimEnv, vm *VirtualMachine, want string) {
	t.Helper()
	waitGuestProperty(t, env, vm.Self, "guest.toolsRunningStatus", want)
	env.simCtx.WithLock(vm, func() {
		require.Equal(t, want, vm.Summary.Guest.ToolsRunningStatus)
	})
}

// stableToolsRunningStatus checks that vm still reports want after the
// server has had time to act on whatever it was last sent.
func stableToolsRunningStatus(t *testing.T, env *vcSimEnv, vm *VirtualMachine, want string) {
	t.Helper()
	time.Sleep(100 * time.Millisecond)
	env.simCtx.WithLock(vm, func() {
		require.Equal(t, want, vm.Guest.ToolsRunningStatus)
	})
}

func resetToolsStatus(env *vcSimEnv, vm *VirtualMachine) {
	env.simCtx.WithLock(vm, func() {
		env.simCtx.Update(vm, toolsNotRunning)
	})
}

// TestGuestTools_Toolbox checks that the toolbox daemon is reported running
// once it has identified itself and while that connection stays open, and
// that one-shot commands do not count.
func TestGuestTools_Toolbox(t *testing.T) {
	env := newVCSIMEnv(t)
	vm := allVMs(t, env)[0]
	srv, oneShot := serveGuestRPC(t, env, vm)
	resetToolsStatus(env, vm)

	_, err := oneShot.Request([]byte("info-set guestinfo.vmtools.versionNumber 2147483647"))
	require.NoError(t, err)
	_, err = oneShot.Request([]byte("info-set guestinfo.other x"))
	require.NoError(t, err)
	stableToolsRunningStatus(t, env, vm, guestToolsNotRunning)

	daemon := dialGuestRPC(t, srv.path)
	_, err = daemon.Request([]byte("info-set guestinfo.vmtools.description " + toolbox.ToolsDescription + " 0.55.0\x00"))
	require.NoError(t, err)
	requireToolsRunningStatus(t, env, vm, guestToolsRunning)

	// A second daemon connection, then the first closing, as across a
	// guest reset.
	second := dialGuestRPC(t, srv.path)
	_, err = second.Request([]byte("info-set guestinfo.vmtools.description " + toolbox.ToolsDescription))
	require.NoError(t, err)
	require.NoError(t, daemon.Stop())
	stableToolsRunningStatus(t, env, vm, guestToolsRunning)

	require.NoError(t, second.Stop())
	requireToolsRunningStatus(t, env, vm, guestToolsNotRunning)
}

// TestGuestTools_Paused checks that a suspended VM's tools are not running,
// and that they are again on resume while the daemon's connection is open.
func TestGuestTools_Paused(t *testing.T) {
	env := newVCSIMEnv(t)
	vm := allVMs(t, env)[0]
	srv, daemon := serveGuestRPC(t, env, vm)

	_, err := daemon.Request([]byte("info-set guestinfo.vmtools.description " + toolbox.ToolsDescription))
	require.NoError(t, err)
	requireToolsRunningStatus(t, env, vm, guestToolsRunning)

	svm := &simVM{vm: vm, guestRPC: srv}
	env.simCtx.WithLock(vm, func() { svm.updateToolsStatus(env.simCtx, true) })
	requireToolsRunningStatus(t, env, vm, guestToolsNotRunning)

	env.simCtx.WithLock(vm, func() { svm.updateToolsStatus(env.simCtx, false) })
	requireToolsRunningStatus(t, env, vm, guestToolsRunning)
}

// TestGuestTools_StoppedServer checks that a connection closing after power
// off does not report the tools again.
func TestGuestTools_StoppedServer(t *testing.T) {
	env := newVCSIMEnv(t)
	vm := allVMs(t, env)[0]
	srv, daemon := serveGuestRPC(t, env, vm)

	_, err := daemon.Request([]byte("info-set guestinfo.vmtools.description " + toolbox.ToolsDescription))
	require.NoError(t, err)
	requireToolsRunningStatus(t, env, vm, guestToolsRunning)

	srv.stop()
	env.simCtx.WithLock(vm, func() { env.simCtx.Update(vm, toolsNotRunning) })
	stableToolsRunningStatus(t, env, vm, guestToolsNotRunning)
}

// TestGuestTools_Heartbeat checks that vmtoolsd is reported running while its
// TCLO heartbeat continues, and not running once it stops. vmtoolsd's
// description alone does not count.
func TestGuestTools_Heartbeat(t *testing.T) {
	saved := toolsHeartbeatTimeout
	toolsHeartbeatTimeout = 200 * time.Millisecond
	t.Cleanup(func() { toolsHeartbeatTimeout = saved })

	env := newVCSIMEnv(t)
	vm := allVMs(t, env)[0]
	srv, rpci := serveGuestRPC(t, env, vm)
	resetToolsStatus(env, vm)

	_, err := rpci.Request([]byte("info-set guestinfo.vmtools.description open-vm-tools 12.5.0 build 24276846\x00"))
	require.NoError(t, err)
	stableToolsRunningStatus(t, env, vm, guestToolsNotRunning)

	tclo, err := net.Dial("unix", srv.path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tclo.Close() })

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		tick := time.NewTicker(50 * time.Millisecond)
		defer tick.Stop()
		for {
			if toolbox.WriteGuestRPCPing(tclo) != nil {
				return
			}
			select {
			case <-stop:
				return
			case <-tick.C:
			}
		}
	}()

	requireToolsRunningStatus(t, env, vm, guestToolsRunning)
	// Several timeouts' worth of heartbeats keep it running.
	time.Sleep(3 * toolsHeartbeatTimeout)
	env.simCtx.WithLock(vm, func() {
		require.Equal(t, guestToolsRunning, vm.Guest.ToolsRunningStatus)
	})

	close(stop)
	<-done
	requireToolsRunningStatus(t, env, vm, guestToolsNotRunning)
}

// TestGuestTools_Version checks tools.set.version and tools.set.versiontype,
// and that the toolbox's unmanaged version passes mobility-operator's
// VMware Tools check (ToolsVersionStatus2 set and not guestToolsNotInstalled).
func TestGuestTools_Version(t *testing.T) {
	env := newVCSIMEnv(t)
	vm := allVMs(t, env)[0]
	_, ch := serveGuestRPC(t, env, vm)

	tests := []struct {
		cmd, version, installType, status string
	}{
		{"tools.set.version 2147483647", "2147483647",
			string(types.VirtualMachineToolsInstallTypeGuestToolsTypeUnknown),
			string(types.VirtualMachineToolsVersionStatusGuestToolsUnmanaged)},
		{"tools.set.versiontype 12896 4\x00", "12896",
			string(types.VirtualMachineToolsInstallTypeGuestToolsTypeOpenVMTools),
			string(types.VirtualMachineToolsVersionStatusGuestToolsUnmanaged)},
		{"tools.set.versiontype 12896 1", "12896",
			string(types.VirtualMachineToolsInstallTypeGuestToolsTypeMSI),
			string(types.VirtualMachineToolsVersionStatusGuestToolsCurrent)},
	}
	for _, tt := range tests {
		t.Run(tt.cmd, func(t *testing.T) {
			_, err := ch.Request([]byte(tt.cmd))
			require.NoError(t, err)
			env.simCtx.WithLock(vm, func() {
				require.Equal(t, tt.version, vm.Guest.ToolsVersion)
				require.Equal(t, tt.installType, vm.Guest.ToolsInstallType)
				require.Equal(t, tt.status, vm.Guest.ToolsVersionStatus)
				require.Equal(t, tt.status, vm.Guest.ToolsVersionStatus2)
				require.Equal(t, tt.status, vm.Summary.Guest.ToolsVersionStatus2)
				require.NotEqual(t, string(types.VirtualMachineToolsVersionStatusGuestToolsNotInstalled), vm.Guest.ToolsVersionStatus2)
			})
		})
	}

	_, err := ch.Request([]byte("tools.set.version x"))
	require.Error(t, err)
}
