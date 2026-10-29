// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package simulator

import (
	"slices"
	"strings"

	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/vim25/types"
)

// FCDAttachSpec is one first-class disk to attach, as AttachDisk_Task and CNS AttachVolume
// ask for it. Zero values take the defaults in the comments.
type FCDAttachSpec struct {
	ID            types.ID
	Datastore     types.ManagedObjectReference // zero: search every datastore
	ControllerKey *int32                       // nil: first SCSI controller with a free unit
	UnitNumber    *int32                       // nil: the controller's next free unit
	DiskMode      string                       // "": persistent
	Sharing       string                       // "": sharingNone
}

// AttachFCD adds the FCD to vm as a VirtualDisk through configureDevices, or returns the disk
// already there. It locks vm, then the VStorageObjectManager.
func (vm *VirtualMachine) AttachFCD(ctx *Context, spec FCDAttachSpec) (disk *types.VirtualDisk, fault types.BaseMethodFault) {
	ctx.WithLock(vm, func() {
		disk, fault = vm.attachFCD(ctx, spec)
	})
	return disk, fault
}

func (vm *VirtualMachine) attachFCD(ctx *Context, spec FCDAttachSpec) (*types.VirtualDisk, types.BaseMethodFault) {
	fcd := vm.fcd(ctx, spec.Datastore, spec.ID)
	if fcd == nil {
		return nil, new(types.InvalidArgument)
	}
	backing := fcd.Config.Backing.(*types.BaseConfigInfoDiskFileBackingInfo)
	ds := backing.Datastore
	devices := object.VirtualDeviceList(vm.Config.Hardware.Device)

	for _, d := range devices.SelectByType((*types.VirtualDisk)(nil)) {
		disk := d.(*types.VirtualDisk)
		if disk.VDiskId != nil && disk.VDiskId.Id == spec.ID.Id {
			return disk, nil
		}
		if disk.VDiskId == nil && diskFileName(disk) == backing.FilePath {
			// A disk added over the FCD's file, e.g. at create time, becomes the FCD's.
			disk.VDiskId = &types.ID{Id: spec.ID.Id}
			addFCDConsumer(ctx, spec.ID, vm.Config.Uuid)
			return disk, nil
		}
	}

	disk := &types.VirtualDisk{
		VirtualDevice: types.VirtualDevice{
			Key: devices.NewKey(),
			Backing: &types.VirtualDiskFlatVer2BackingInfo{
				VirtualDeviceFileBackingInfo: types.VirtualDeviceFileBackingInfo{
					FileName:  backing.FilePath,
					Datastore: &ds,
				},
				DiskMode:        orDefault(spec.DiskMode, string(types.VirtualDiskModePersistent)),
				Sharing:         orDefault(spec.Sharing, string(types.VirtualDiskSharingSharingNone)),
				ThinProvisioned: types.NewBool(backing.ProvisioningType == string(types.BaseConfigInfoDiskFileBackingInfoProvisioningTypeThin)),
			},
		},
		CapacityInKB:    fcd.Config.CapacityInMB * 1024,
		CapacityInBytes: fcd.Config.CapacityInMB * 1024 * 1024,
		VDiskId:         &types.ID{Id: spec.ID.Id},
	}
	if fault := placeFCD(devices, &disk.VirtualDevice, spec); fault != nil {
		return nil, fault
	}

	fault := vm.configureDevices(ctx, &types.VirtualMachineConfigSpec{
		DeviceChange: []types.BaseVirtualDeviceConfigSpec{&types.VirtualDeviceConfigSpec{
			Operation: types.VirtualDeviceConfigSpecOperationAdd,
			Device:    disk,
		}},
	})
	if fault != nil {
		return nil, fault
	}
	return disk, nil
}

func orDefault(val, def string) string {
	if val == "" {
		return def
	}
	return val
}

// placeFCD sets the disk's controller and unit: the request's verbatim, else the first SCSI
// controller by key with a free unit, picked as VirtualDeviceList.AssignController does.
func placeFCD(devices object.VirtualDeviceList, d *types.VirtualDevice, spec FCDAttachSpec) types.BaseMethodFault {
	var controllers []types.BaseVirtualController
	if spec.ControllerKey != nil {
		if c, ok := devices.FindByKey(*spec.ControllerKey).(types.BaseVirtualController); ok {
			controllers = append(controllers, c)
		}
	} else {
		for _, c := range devices.SelectByType((*types.VirtualSCSIController)(nil)) {
			controllers = append(controllers, c.(types.BaseVirtualController))
		}
		slices.SortFunc(controllers, func(a, b types.BaseVirtualController) int {
			return int(a.GetVirtualController().Key - b.GetVirtualController().Key)
		})
	}

	for _, c := range controllers {
		unit := spec.UnitNumber
		if unit == nil {
			if free := freeUnit(devices, c); free >= 0 {
				unit = &free
			}
		}
		if unit != nil {
			d.ControllerKey = c.GetVirtualController().Key
			d.UnitNumber = types.NewInt32(*unit)
			return nil
		}
	}

	return new(types.MissingController)
}

// freeUnit returns the controller's lowest free unit, or -1.
func freeUnit(devices object.VirtualDeviceList, c types.BaseVirtualController) int32 {
	units := make([]bool, 16)
	if sc, ok := c.(types.BaseVirtualSCSIController); ok {
		units[sc.GetVirtualSCSIController().ScsiCtlrUnitNumber] = true
	}
	key := c.GetVirtualController().Key
	for _, device := range devices {
		d := device.GetVirtualDevice()
		if d.ControllerKey == key && d.UnitNumber != nil && int(*d.UnitNumber) < len(units) {
			units[*d.UnitNumber] = true
		}
	}
	for unit, used := range units {
		if !used {
			return int32(unit)
		}
	}
	return -1
}

// DetachFCD removes vm's VirtualDisk for the FCD, keeping the FCD's files.
func (vm *VirtualMachine) DetachFCD(ctx *Context, id types.ID) (fault types.BaseMethodFault) {
	ctx.WithLock(vm, func() {
		if vm.fcd(ctx, types.ManagedObjectReference{}, id) == nil {
			fault = new(types.InvalidArgument)
			return
		}
		disk := vm.fcdDisk(id)
		if disk == nil {
			fault = &types.NotFound{}
			return
		}
		fault = vm.configureDevices(ctx, &types.VirtualMachineConfigSpec{
			DeviceChange: []types.BaseVirtualDeviceConfigSpec{&types.VirtualDeviceConfigSpec{
				Operation: types.VirtualDeviceConfigSpecOperationRemove,
				Device:    disk,
			}},
		})
	})
	return fault
}

// fcdDisk returns vm's VirtualDisk whose VDiskId is id, or nil.
func (vm *VirtualMachine) fcdDisk(id types.ID) *types.VirtualDisk {
	for _, d := range object.VirtualDeviceList(vm.Config.Hardware.Device).SelectByType((*types.VirtualDisk)(nil)) {
		if disk := d.(*types.VirtualDisk); disk.VDiskId != nil && disk.VDiskId.Id == id.Id {
			return disk
		}
	}
	return nil
}

// FCDHolders returns the VMs whose config.uuid is in the FCD's ConsumerId.
func FCDHolders(ctx *Context, id types.ID) []*VirtualMachine {
	m := vStorageObjectManager(ctx)
	if m == nil {
		return nil
	}
	var consumers []string
	ctx.WithLock(m, func() {
		for _, set := range m.objects {
			if obj, ok := set[id]; ok {
				for _, c := range obj.Config.ConsumerId {
					consumers = append(consumers, c.Id)
				}
			}
		}
	})

	var holders []*VirtualMachine
	for _, e := range ctx.Map.All("VirtualMachine") {
		if vm := e.(*VirtualMachine); slices.Contains(consumers, vm.Config.Uuid) {
			holders = append(holders, vm)
		}
	}
	return holders
}

// DiskUUID is a disk's backing UUID as CNS reports it: lowercase, no dashes.
func DiskUUID(disk *types.VirtualDisk) string {
	b, ok := disk.Backing.(*types.VirtualDiskFlatVer2BackingInfo)
	if !ok {
		return ""
	}
	return strings.ToLower(strings.ReplaceAll(b.Uuid, "-", ""))
}

// diskHoldMode is how a holder uses a disk's backing: shared holders coexist, an exclusive
// holder excludes every other.
type diskHoldMode int

const (
	diskHoldExclusive diskHoldMode = iota
	diskHoldShared
)

// holdMode is shared for multi-writer sharing and for nonpersistent modes, which never write
// the base file.
func holdMode(disk *types.VirtualDisk) diskHoldMode {
	b, ok := disk.Backing.(*types.VirtualDiskFlatVer2BackingInfo)
	if !ok {
		return diskHoldExclusive
	}
	switch {
	case b.Sharing == string(types.VirtualDiskSharingSharingMultiWriter),
		b.DiskMode == string(types.VirtualDiskModeNonpersistent),
		b.DiskMode == string(types.VirtualDiskModeIndependent_nonpersistent):
		return diskHoldShared
	}
	return diskHoldExclusive
}

// diskHold is one registered VM's use of a disk backing.
type diskHold struct {
	vm   *VirtualMachine
	mode diskHoldMode
}

// diskHolders returns the registered VMs whose devices include a disk over the file.
func diskHolders(ctx *Context, file string) []diskHold {
	var holds []diskHold
	for _, e := range ctx.Map.All("VirtualMachine") {
		vm := e.(*VirtualMachine)
		for _, d := range object.VirtualDeviceList(vm.Config.Hardware.Device).SelectByType((*types.VirtualDisk)(nil)) {
			if disk := d.(*types.VirtualDisk); diskFileName(disk) == file {
				holds = append(holds, diskHold{vm, holdMode(disk)})
			}
		}
	}
	return holds
}

// checkExclusive returns ResourceInUse if vm may not hold the file in mode beside its other
// holders. Slice 1 applies it to FCD-backed disks only.
func checkExclusive(ctx *Context, file string, vm *VirtualMachine, mode diskHoldMode, name string) types.BaseMethodFault {
	for _, h := range diskHolders(ctx, file) {
		if h.vm != vm && (mode == diskHoldExclusive || h.mode == diskHoldExclusive) {
			return &types.ResourceInUse{Name: name}
		}
	}
	return nil
}

// checkFCDHold applies checkExclusive to a disk being added, if its backing is an FCD's.
func (vm *VirtualMachine) checkFCDHold(ctx *Context, disk *types.VirtualDisk) types.BaseMethodFault {
	file := diskFileName(disk)
	id, ok := fcdByFile(ctx, file)
	if !ok {
		return nil
	}
	return checkExclusive(ctx, file, vm, holdMode(disk), id.Id)
}

func diskFileName(disk *types.VirtualDisk) string {
	if b, ok := disk.Backing.(types.BaseVirtualDeviceFileBackingInfo); ok {
		return b.GetVirtualDeviceFileBackingInfo().FileName
	}
	return ""
}

func vStorageObjectManager(ctx *Context) *VcenterVStorageObjectManager {
	ref := ctx.Map.content().VStorageObjectManager
	if ref == nil {
		return nil
	}
	m, _ := ctx.Map.Get(*ref).(*VcenterVStorageObjectManager)
	return m
}

// fcdByFile returns the id of the registered FCD whose backing is the file.
func fcdByFile(ctx *Context, file string) (id types.ID, found bool) {
	m := vStorageObjectManager(ctx)
	if m == nil || file == "" {
		return id, false
	}
	ctx.WithLock(m, func() {
		for _, set := range m.objects {
			for key, obj := range set {
				if b, ok := obj.Config.Backing.(*types.BaseConfigInfoDiskFileBackingInfo); ok && b.FilePath == file {
					id, found = key, true
					return
				}
			}
		}
	})
	return id, found
}

// addFCDConsumer and removeFCDConsumer keep the FCD's ConsumerId the mirror of its holders'
// device lists, under the VStorageObjectManager lock.
func addFCDConsumer(ctx *Context, id types.ID, uuid string) {
	updateFCDConsumers(ctx, func(fcd types.ID, consumers []types.ID) []types.ID {
		if fcd.Id != id.Id || slices.Contains(consumers, types.ID{Id: uuid}) {
			return consumers
		}
		return append(consumers, types.ID{Id: uuid})
	})
}

func removeFCDConsumer(ctx *Context, id *types.ID, uuid string) {
	updateFCDConsumers(ctx, func(fcd types.ID, consumers []types.ID) []types.ID {
		if id != nil && fcd.Id != id.Id {
			return consumers
		}
		return slices.DeleteFunc(consumers, func(c types.ID) bool { return c.Id == uuid })
	})
}

func updateFCDConsumers(ctx *Context, update func(types.ID, []types.ID) []types.ID) {
	m := vStorageObjectManager(ctx)
	if m == nil {
		return
	}
	ctx.WithLock(m, func() {
		for _, set := range m.objects {
			for id, obj := range set {
				obj.Config.ConsumerId = update(id, obj.Config.ConsumerId)
				if len(obj.Config.ConsumerId) == 0 {
					obj.Config.ConsumerId = nil
				}
			}
		}
	})
}
