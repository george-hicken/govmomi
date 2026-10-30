// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package simulator

import (
	"fmt"
	"slices"

	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/vim25/types"
)

// diskAccessFault returns the fault vSphere raises when one of vm's virtual disks is on a
// datastore that is not mounted and accessible on host: CannotAccessVmDisk ("One of the virtual
// machine's virtual disks is not accessible"), a VmConfigFault, wrapping InaccessibleDatastore.
// It returns nil when every disk can be reached from host.
func (vm *VirtualMachine) diskAccessFault(ctx *Context, host *HostSystem) types.BaseMethodFault {
	for _, d := range vm.Config.Hardware.Device {
		disk, ok := d.(*types.VirtualDisk)
		if !ok {
			continue
		}
		b, ok := disk.Backing.(types.BaseVirtualDeviceFileBackingInfo)
		if !ok {
			continue
		}
		backing := b.GetVirtualDeviceFileBackingInfo()
		ds := vm.backingDatastore(ctx, backing)
		if ds == nil || datastoreAccessibleOn(ds, host) {
			continue
		}
		return &types.CannotAccessVmDisk{
			CannotAccessVmDevice: types.CannotAccessVmDevice{
				Device:    disk.DeviceInfo.GetDescription().Label,
				Backing:   backing.FileName,
				Connected: disk.Connectable == nil || disk.Connectable.Connected,
			},
			Fault: types.LocalizedMethodFault{
				Fault: &types.InaccessibleDatastore{
					InvalidDatastore: types.InvalidDatastore{Datastore: &ds.Self, Name: ds.Name},
					Detail:           fmt.Sprintf("not mounted and accessible on host %s", host.Name),
				},
				LocalizedMessage: fmt.Sprintf("datastore %s is not accessible on host %s", ds.Name, host.Name),
			},
		}
	}
	return nil
}

// backingDatastore resolves the datastore of a file backing, by reference or else by the
// datastore name in its path.
func (vm *VirtualMachine) backingDatastore(ctx *Context, backing *types.VirtualDeviceFileBackingInfo) *Datastore {
	if backing.Datastore != nil {
		ds, _ := ctx.Map.Get(*backing.Datastore).(*Datastore)
		return ds
	}
	var p object.DatastorePath
	if !p.FromString(backing.FileName) {
		return nil
	}
	ds, _ := ctx.Map.FindByName(p.Datastore, vm.Datastore).(*Datastore)
	return ds
}

// datastoreAccessibleOn reports whether host lists ds and ds's mount on host is mounted and
// accessible.
func datastoreAccessibleOn(ds *Datastore, host *HostSystem) bool {
	if !slices.Contains(host.Datastore, ds.Self) {
		return false
	}
	for _, m := range ds.Host {
		if m.Key == host.Self {
			info := m.MountInfo
			return isTrue(info.Mounted) && isTrue(info.Accessible)
		}
	}
	return false
}
