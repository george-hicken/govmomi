// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package simulator

import (
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vmware/govmomi/toolbox"
	"github.com/vmware/govmomi/vim25/types"
)

// toolsHeartbeatTimeout is how long after vmtoolsd's last TCLO heartbeat its
// tools are reported running. vmtoolsd sends one every second
// (lib/rpcIn/rpcin.c RPCIN_HEARTBEAT_INTERVAL); the VMX's own timeout is not
// published, so this is an estimate.
var toolsHeartbeatTimeout = 5 * time.Second

// toolsVersionUnmanaged is TOOLS_VERSION_UNMANAGED from open-vm-tools
// lib/include/vm_tools_version.h: tools whose version the host does not manage.
const toolsVersionUnmanaged = 0x7fffffff

// guestTools is the state of one VM's guest tools during one power-on, from
// which guest.toolsRunningStatus is derived.
//
// The two kinds of tools are told apart by the guestinfo.vmtools.description
// they report when they start. The govmomi toolbox has no heartbeat, so it is
// running while a connection on which it reported itself is open; vmtoolsd's
// one-shot commands (vmware-rpctool, vmtoolsd --cmd) do not report it and so
// do not count. vmtoolsd is running while its TCLO heartbeat continues.
//
// The fields are atomic so that the status, computed under the VM's lock,
// can be read from a power operation that already holds that lock.
type guestTools struct {
	daemons  atomic.Int32 // open connections from the toolbox daemon
	beating  atomic.Bool  // a heartbeat arrived within toolsHeartbeatTimeout
	lastBeat atomic.Int64 // UnixNano of the last heartbeat
	paused   atomic.Bool  // the VM is suspended

	mu    sync.Mutex
	timer *time.Timer // expires the heartbeat
}

// running reports whether the tools are running.
func (t *guestTools) running() bool {
	return !t.paused.Load() && (t.daemons.Load() > 0 || t.beating.Load())
}

// isToolboxDescription reports whether cmd is the govmomi toolbox reporting
// its version description.
func isToolboxDescription(cmd string) bool {
	return strings.HasPrefix(strings.TrimRight(cmd, "\x00"),
		"info-set guestinfo.vmtools.description "+toolbox.ToolsDescription)
}

// toolsStatusChanges returns the property changes that report the tools as
// running or not.
func toolsStatusChanges(running bool) []types.PropertyChange {
	if running {
		return toolsRunning
	}
	return toolsNotRunning
}

// syncToolsStatus reports the tools' current state on the VM, unless the
// server has stopped, in which case the power operation that stopped it has
// reported it.
func (s *guestRPCServer) syncToolsStatus(ctx *Context) {
	ctx.WithLock(s.vm, func() {
		select {
		case <-s.done:
			return
		default:
		}
		ctx.Update(s.vm, toolsStatusChanges(s.tools.running()))
	})
}

// setPaused records whether the VM is suspended. The caller, a power
// operation, reports the resulting status.
func (s *guestRPCServer) setPaused(paused bool) {
	s.tools.paused.Store(paused)
}

// heartbeat records one vmtoolsd TCLO heartbeat.
func (s *guestRPCServer) heartbeat(ctx *Context) {
	s.tools.lastBeat.Store(time.Now().UnixNano())

	s.tools.mu.Lock()
	if s.tools.timer == nil {
		s.tools.timer = time.AfterFunc(toolsHeartbeatTimeout, s.heartbeatExpired)
	} else {
		s.tools.timer.Reset(toolsHeartbeatTimeout)
	}
	s.tools.mu.Unlock()

	if !s.tools.beating.Swap(true) {
		s.syncToolsStatus(ctx)
	}
}

// heartbeatExpired reports the tools not running once no heartbeat has
// arrived for toolsHeartbeatTimeout.
func (s *guestRPCServer) heartbeatExpired() {
	since := time.Since(time.Unix(0, s.tools.lastBeat.Load()))
	if since < toolsHeartbeatTimeout {
		return // a heartbeat raced the timer, which it has reset
	}
	if s.tools.beating.Swap(false) {
		s.syncToolsStatus(s.connContext())
	}
}

func (s *guestRPCServer) stopHeartbeat() {
	s.tools.mu.Lock()
	defer s.tools.mu.Unlock()
	if s.tools.timer != nil {
		s.tools.timer.Stop()
	}
}

// setToolsVersion handles "tools.set.versiontype <version> <type>" and
// "tools.set.version <version>", with which tools report their version
// (open-vm-tools services/vmtoolsd/toolsRpc.c ToolsCoreRpcCapReg). args is
// what follows the command.
func (s *guestRPCServer) setToolsVersion(ctx *Context, args string) string {
	fields := strings.Fields(strings.TrimRight(args, "\x00"))
	if len(fields) == 0 || len(fields) > 2 {
		return "0 Bad syntax"
	}
	version, err := strconv.ParseUint(fields[0], 10, 32)
	if err != nil {
		return "0 Bad syntax"
	}
	kind := uint64(0) // TOOLS_TYPE_UNKNOWN
	if len(fields) == 2 {
		if kind, err = strconv.ParseUint(fields[1], 10, 32); err != nil {
			return "0 Bad syntax"
		}
	}

	installType := toolsInstallType(kind)
	status := types.VirtualMachineToolsVersionStatusGuestToolsCurrent
	if version == toolsVersionUnmanaged ||
		installType == types.VirtualMachineToolsInstallTypeGuestToolsTypeOpenVMTools ||
		installType == types.VirtualMachineToolsInstallTypeGuestToolsTypeOSP {
		// The guest, not the host, manages these tools.
		status = types.VirtualMachineToolsVersionStatusGuestToolsUnmanaged
	}

	ctx.AutoUpdate(s.vm, func() {
		s.vm.Guest.ToolsVersion = strconv.FormatUint(version, 10)
		s.vm.Guest.ToolsInstallType = string(installType)
		s.vm.Guest.ToolsVersionStatus = string(status)
		s.vm.Guest.ToolsVersionStatus2 = string(status)
		s.vm.Summary.Guest.ToolsVersionStatus = string(status)
		s.vm.Summary.Guest.ToolsVersionStatus2 = string(status)
	})
	return "1 "
}

// toolsInstallType maps a ToolsType from open-vm-tools
// lib/include/vm_tools_version.h to guest.toolsInstallType.
func toolsInstallType(kind uint64) types.VirtualMachineToolsInstallType {
	switch kind {
	case 1: // TOOLS_TYPE_MSI
		return types.VirtualMachineToolsInstallTypeGuestToolsTypeMSI
	case 2: // TOOLS_TYPE_TARBALL
		return types.VirtualMachineToolsInstallTypeGuestToolsTypeTar
	case 3: // TOOLS_TYPE_OSP
		return types.VirtualMachineToolsInstallTypeGuestToolsTypeOSP
	case 4: // TOOLS_TYPE_OVT
		return types.VirtualMachineToolsInstallTypeGuestToolsTypeOpenVMTools
	default: // TOOLS_TYPE_UNKNOWN
		return types.VirtualMachineToolsInstallTypeGuestToolsTypeUnknown
	}
}
