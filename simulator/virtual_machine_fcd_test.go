// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package simulator

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/task"
	"github.com/vmware/govmomi/vim25"
	"github.com/vmware/govmomi/vim25/methods"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
)

// fcdEnv drives FCD attach and detach through the vim API of a VPX model.
type fcdEnv struct {
	t     *testing.T
	ctx   context.Context
	c     *vim25.Client
	model *Model
	ds    *Datastore
}

func testFCD(t *testing.T, f func(*fcdEnv)) {
	t.Helper()
	m := VPX()
	defer m.Remove()
	err := m.Run(func(ctx context.Context, c *vim25.Client) error {
		f(&fcdEnv{t: t, ctx: ctx, c: c, model: m, ds: m.Map().Any("Datastore").(*Datastore)})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// vm returns the model's i'th VM, powered off.
func (e *fcdEnv) vm(i int) *VirtualMachine {
	e.t.Helper()
	vms := e.model.Map().All("VirtualMachine")
	slices.SortFunc(vms, func(a, b mo.Entity) int { return strings.Compare(a.Entity().Name, b.Entity().Name) })
	vm := vms[i].(*VirtualMachine)
	if vm.Runtime.PowerState == types.VirtualMachinePowerStatePoweredOn {
		e.wait(object.NewVirtualMachine(e.c, vm.Self).PowerOff(e.ctx))
	}
	return vm
}

func (e *fcdEnv) wait(t *object.Task, err error) error {
	if err != nil {
		return err
	}
	return t.Wait(e.ctx)
}

func (e *fcdEnv) createFCD(name string) types.ID {
	e.t.Helper()
	res, err := methods.CreateDisk_Task(e.ctx, e.c, &types.CreateDisk_Task{
		This: *e.c.ServiceContent.VStorageObjectManager,
		Spec: types.VslmCreateSpec{
			Name:         name,
			CapacityInMB: 10,
			BackingSpec: &types.VslmCreateSpecDiskFileBackingSpec{
				VslmCreateSpecBackingSpec: types.VslmCreateSpecBackingSpec{Datastore: e.ds.Self},
				ProvisioningType:          string(types.BaseConfigInfoDiskFileBackingInfoProvisioningTypeThin),
			},
		},
	})
	if err != nil {
		e.t.Fatal(err)
	}
	info, err := object.NewTask(e.c, res.Returnval).WaitForResult(e.ctx)
	if err != nil {
		e.t.Fatal(err)
	}
	return info.Result.(types.VStorageObject).Config.Id
}

func (e *fcdEnv) fcd(id types.ID) *VStorageObject {
	return e.model.Map().VStorageObjectManager().object(e.ds.Self, id)
}

func (e *fcdEnv) fcdPath(id types.ID) string {
	return e.fcd(id).Config.Backing.(*types.BaseConfigInfoDiskFileBackingInfo).FilePath
}

func (e *fcdEnv) consumers(id types.ID) []string {
	var ids []string
	for _, c := range e.fcd(id).Config.ConsumerId {
		ids = append(ids, c.Id)
	}
	return ids
}

func (e *fcdEnv) attach(vm *VirtualMachine, id types.ID, controllerKey int32, unit *int32) error {
	return object.NewVirtualMachine(e.c, vm.Self).AttachDisk(e.ctx, id.Id, object.NewDatastore(e.c, e.ds.Self), controllerKey, unit)
}

func (e *fcdEnv) detach(vm *VirtualMachine, id types.ID) error {
	return object.NewVirtualMachine(e.c, vm.Self).DetachDisk(e.ctx, id.Id)
}

func (e *fcdEnv) reconfigure(vm *VirtualMachine, specs ...types.BaseVirtualDeviceConfigSpec) error {
	return e.wait(object.NewVirtualMachine(e.c, vm.Self).Reconfigure(e.ctx, types.VirtualMachineConfigSpec{DeviceChange: specs}))
}

// addDisk adds, by ReconfigVM_Task, a disk over the FCD's file on vm's first SCSI controller.
func (e *fcdEnv) addDisk(vm *VirtualMachine, id *types.ID, file, mode, sharing string) error {
	devices := object.VirtualDeviceList(vm.Config.Hardware.Device)
	c, err := devices.FindSCSIController("")
	if err != nil {
		e.t.Fatal(err)
	}
	disk := &types.VirtualDisk{
		VirtualDevice: types.VirtualDevice{
			Backing: &types.VirtualDiskFlatVer2BackingInfo{
				VirtualDeviceFileBackingInfo: types.VirtualDeviceFileBackingInfo{FileName: file},
				DiskMode:                     mode,
				Sharing:                      sharing,
			},
		},
		CapacityInKB: 10 * 1024,
		VDiskId:      id,
	}
	devices.AssignController(disk, c)
	return e.reconfigure(vm, &types.VirtualDeviceConfigSpec{Operation: types.VirtualDeviceConfigSpecOperationAdd, Device: disk})
}

func fcdDisks(vm *VirtualMachine) []*types.VirtualDisk {
	var disks []*types.VirtualDisk
	for _, d := range object.VirtualDeviceList(vm.Config.Hardware.Device).SelectByType((*types.VirtualDisk)(nil)) {
		disks = append(disks, d.(*types.VirtualDisk))
	}
	return disks
}

func faultOf(err error) types.BaseMethodFault {
	var te task.Error
	if errors.As(err, &te) {
		return te.Fault()
	}
	return nil
}

func requireFault[T types.BaseMethodFault](t *testing.T, err error) {
	t.Helper()
	if _, ok := faultOf(err).(T); !ok {
		var want T
		t.Fatalf("want %T, got %v", want, err)
	}
}

func (e *fcdEnv) fileExists(path string) bool {
	var p object.DatastorePath
	p.FromString(path)
	_, err := os.Stat(e.ds.resolve(e.model.Service.Context, p.Path))
	return err == nil
}

func TestAttachFCD(t *testing.T) {
	t.Run("T1 attach a new FCD", func(t *testing.T) {
		testFCD(t, func(e *fcdEnv) {
			vm := e.vm(0)
			id := e.createFCD("t1")
			n, disks := vm.Summary.Config.NumVirtualDisks, len(fcdDisks(vm))
			if err := e.attach(vm, id, 0, nil); err != nil {
				t.Fatal(err)
			}
			if len(fcdDisks(vm)) != disks+1 || vm.Summary.Config.NumVirtualDisks != n+1 {
				t.Fatalf("want one new disk, have %d (numVirtualDisks %d)", len(fcdDisks(vm)), vm.Summary.Config.NumVirtualDisks)
			}
			disk := vm.fcdDisk(id)
			b := disk.Backing.(*types.VirtualDiskFlatVer2BackingInfo)
			switch {
			case disk.VDiskId == nil || disk.VDiskId.Id != id.Id:
				t.Errorf("VDiskId %v", disk.VDiskId)
			case b.FileName != e.fcdPath(id):
				t.Errorf("FileName %s", b.FileName)
			case disk.CapacityInKB != e.fcd(id).Config.CapacityInMB*1024:
				t.Errorf("CapacityInKB %d", disk.CapacityInKB)
			case b.Uuid == "":
				t.Error("no Uuid")
			}
			if c := e.consumers(id); len(c) != 1 || c[0] != vm.Config.Uuid {
				t.Errorf("ConsumerId %v", c)
			}
		})
	})

	t.Run("T2 two FCDs on one VM skip unit 7", func(t *testing.T) {
		testFCD(t, func(e *fcdEnv) {
			vm := e.vm(0)
			uuids := map[string]bool{}
			units := map[int32]bool{}
			var controller int32
			for i := range 7 {
				id := e.createFCD("t2-" + string(rune('a'+i)))
				if err := e.attach(vm, id, 0, nil); err != nil {
					t.Fatal(err)
				}
				disk := vm.fcdDisk(id)
				if i > 0 && disk.ControllerKey != controller {
					t.Errorf("controller %d, want %d", disk.ControllerKey, controller)
				}
				controller = disk.ControllerKey
				uuids[disk.Backing.(*types.VirtualDiskFlatVer2BackingInfo).Uuid] = true
				units[*disk.UnitNumber] = true
			}
			if len(uuids) != 7 || len(units) != 7 || units[7] {
				t.Errorf("uuids %v units %v", uuids, units)
			}
		})
	})

	t.Run("T3 same attach twice", func(t *testing.T) {
		testFCD(t, func(e *fcdEnv) {
			vm := e.vm(0)
			id := e.createFCD("t3")
			if err := e.attach(vm, id, 0, nil); err != nil {
				t.Fatal(err)
			}
			n, disk := len(fcdDisks(vm)), *vm.fcdDisk(id)
			if err := e.attach(vm, id, 0, nil); err != nil {
				t.Fatal(err)
			}
			again := vm.fcdDisk(id)
			if len(fcdDisks(vm)) != n || again.Key != disk.Key || DiskUUID(again) != DiskUUID(&disk) {
				t.Error("retry changed the VM")
			}
		})
	})

	t.Run("T4 disk added over the FCD file adopts it", func(t *testing.T) {
		testFCD(t, func(e *fcdEnv) {
			vm := e.vm(0)
			id := e.createFCD("t4")
			if err := e.addDisk(vm, nil, e.fcdPath(id), "persistent", ""); err != nil {
				t.Fatal(err)
			}
			n := len(fcdDisks(vm))
			if err := e.attach(vm, id, 0, nil); err != nil {
				t.Fatal(err)
			}
			if len(fcdDisks(vm)) != n || vm.fcdDisk(id) == nil || len(e.consumers(id)) != 1 {
				t.Errorf("disks %d, VDiskId set %t, ConsumerId %v", len(fcdDisks(vm)), vm.fcdDisk(id) != nil, e.consumers(id))
			}
		})
	})

	t.Run("T5 placement", func(t *testing.T) {
		testFCD(t, func(e *fcdEnv) {
			vm := e.vm(0)
			existing := fcdDisks(vm)[0]
			id := e.createFCD("t5")
			if err := e.attach(vm, id, existing.ControllerKey, types.NewInt32(9)); err != nil {
				t.Fatal(err)
			}
			if d := vm.fcdDisk(id); d.ControllerKey != existing.ControllerKey || *d.UnitNumber != 9 {
				t.Errorf("placed at %d:%d", d.ControllerKey, *d.UnitNumber)
			}

			taken := e.createFCD("t5-taken")
			requireFault[*types.InvalidDeviceSpec](t, e.attach(vm, taken, existing.ControllerKey, existing.UnitNumber))

			// Remove every disk and SCSI controller, keeping files
			var specs []types.BaseVirtualDeviceConfigSpec
			for _, d := range vm.Config.Hardware.Device {
				switch d.(type) {
				case *types.VirtualDisk, types.BaseVirtualSCSIController:
					specs = append(specs, &types.VirtualDeviceConfigSpec{Operation: types.VirtualDeviceConfigSpecOperationRemove, Device: d})
				}
			}
			if err := e.reconfigure(vm, specs...); err != nil {
				t.Fatal(err)
			}
			requireFault[*types.MissingController](t, e.attach(vm, taken, 0, nil))
		})
	})

	t.Run("T6 attach to a second VM", func(t *testing.T) {
		testFCD(t, func(e *fcdEnv) {
			a, b := e.vm(0), e.vm(1)
			id := e.createFCD("t6")
			if err := e.attach(a, id, 0, nil); err != nil {
				t.Fatal(err)
			}
			n := len(b.Config.Hardware.Device)
			requireFault[*types.ResourceInUse](t, e.attach(b, id, 0, nil))
			if len(b.Config.Hardware.Device) != n || len(e.consumers(id)) != 1 {
				t.Error("conflicting attach changed state")
			}
		})
	})

	for _, tc := range []struct{ name, mode, sharing string }{
		{"T7 multi-writer on two VMs", "persistent", string(types.VirtualDiskSharingSharingMultiWriter)},
		{"T13 nonpersistent on two VMs", string(types.VirtualDiskModeNonpersistent), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testFCD(t, func(e *fcdEnv) {
				id := e.createFCD("shared")
				for i := range 2 {
					if err := e.addDisk(e.vm(i), &id, e.fcdPath(id), tc.mode, tc.sharing); err != nil {
						t.Fatal(err)
					}
				}
				if c := e.consumers(id); len(c) != 2 {
					t.Errorf("ConsumerId %v", c)
				}
				// An exclusive holder may not join shared ones
				requireFault[*types.ResourceInUse](t, e.addDisk(e.vm(2), &id, e.fcdPath(id), "persistent", ""))
			})
		})
	}

	t.Run("T8 detach keeps files and space", func(t *testing.T) {
		testFCD(t, func(e *fcdEnv) {
			vm := e.vm(0)
			id := e.createFCD("t8")
			free, n := e.ds.Summary.FreeSpace, vm.Summary.Config.NumVirtualDisks
			if err := e.attach(vm, id, 0, nil); err != nil {
				t.Fatal(err)
			}
			if err := e.detach(vm, id); err != nil {
				t.Fatal(err)
			}
			path := e.fcdPath(id)
			switch {
			case vm.fcdDisk(id) != nil:
				t.Error("device remains")
			case vm.Summary.Config.NumVirtualDisks != n:
				t.Errorf("numVirtualDisks %d, want %d", vm.Summary.Config.NumVirtualDisks, n)
			case !e.fileExists(path) || !e.fileExists(strings.Replace(path, ".vmdk", "-flat.vmdk", 1)):
				t.Error("FCD files removed")
			case len(e.consumers(id)) != 0:
				t.Errorf("ConsumerId %v", e.consumers(id))
			case e.ds.Summary.FreeSpace != free:
				t.Errorf("FreeSpace %d, want %d", e.ds.Summary.FreeSpace, free)
			}
		})
	})

	t.Run("T9 detach faults", func(t *testing.T) {
		testFCD(t, func(e *fcdEnv) {
			vm := e.vm(0)
			requireFault[*types.NotFound](t, e.detach(vm, e.createFCD("t9")))
			requireFault[*types.InvalidArgument](t, e.detach(vm, types.ID{Id: "unknown"}))
		})
	})

	t.Run("T10 ReconfigVM removes the FCD disk", func(t *testing.T) {
		testFCD(t, func(e *fcdEnv) {
			vm := e.vm(0)
			id := e.createFCD("t10")
			if err := e.attach(vm, id, 0, nil); err != nil {
				t.Fatal(err)
			}
			if err := e.reconfigure(vm, &types.VirtualDeviceConfigSpec{Operation: types.VirtualDeviceConfigSpecOperationRemove, Device: vm.fcdDisk(id)}); err != nil {
				t.Fatal(err)
			}
			if c := e.consumers(id); len(c) != 0 {
				t.Errorf("ConsumerId %v", c)
			}
		})
	})

	t.Run("T11 destroy keeps the FCD", func(t *testing.T) {
		testFCD(t, func(e *fcdEnv) {
			a, b := e.vm(0), e.vm(1)
			id := e.createFCD("t11")
			if err := e.attach(a, id, 0, nil); err != nil {
				t.Fatal(err)
			}
			if err := e.wait(object.NewVirtualMachine(e.c, a.Self).Destroy(e.ctx)); err != nil {
				t.Fatal(err)
			}
			if !e.fileExists(e.fcdPath(id)) || len(e.consumers(id)) != 0 {
				t.Fatalf("files kept %t, ConsumerId %v", e.fileExists(e.fcdPath(id)), e.consumers(id))
			}
			if err := e.attach(b, id, 0, nil); err != nil {
				t.Fatal(err)
			}
		})
	})

	t.Run("T12 delete while attached", func(t *testing.T) {
		testFCD(t, func(e *fcdEnv) {
			vm := e.vm(0)
			id := e.createFCD("t12")
			if err := e.attach(vm, id, 0, nil); err != nil {
				t.Fatal(err)
			}
			del := func() error {
				res, err := methods.DeleteVStorageObject_Task(e.ctx, e.c, &types.DeleteVStorageObject_Task{
					This: *e.c.ServiceContent.VStorageObjectManager, Id: id, Datastore: e.ds.Self,
				})
				if err != nil {
					return err
				}
				return object.NewTask(e.c, res.Returnval).Wait(e.ctx)
			}
			requireFault[*types.InvalidState](t, del())
			if err := e.detach(vm, id); err != nil {
				t.Fatal(err)
			}
			if err := del(); err != nil {
				t.Fatal(err)
			}
		})
	})
}
