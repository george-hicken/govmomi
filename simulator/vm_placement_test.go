// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package simulator

import (
	"context"
	"testing"

	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/task"
	"github.com/vmware/govmomi/vim25/methods"
	"github.com/vmware/govmomi/vim25/types"
)

// TestNoRunningVMWithoutItsDisks is the class check for FU-95113-122: a vcsim operation that
// puts a running VM on a host that cannot reach its disks' datastores. Each row runs one method
// that powers a VM on or sets runtime.host against a host where the VM's datastore is not
// mounted and accessible, and then checks the constraint: no powered-on VM runs on a host
// without its disks' datastores. Methods that power on must also fail with CannotAccessVmDisk.
// The control row powers the VM on without the check and must break the constraint.
func TestNoRunningVMWithoutItsDisks(t *testing.T) {
	type env struct {
		ctx  context.Context
		vm   *VirtualMachine
		sim  *Context
		c    *object.VirtualMachine
		dst  *HostSystem // a host that cannot reach the VM's datastore
		mvm  *object.VirtualMachine
		host *object.HostSystem
	}
	// wait runs a task and returns its fault.
	wait := func(e env, task *object.Task, err error) error {
		if err != nil {
			return err
		}
		return task.Wait(e.ctx)
	}
	powerOn := func(e env) error {
		task, err := e.c.PowerOn(e.ctx)
		return wait(e, task, err)
	}

	tests := []struct {
		name    string
		running bool // the VM is powered on and on another host before op
		op      func(env) error
		refuse  bool // op must fail with CannotAccessVmDisk
		control bool // op skips the check, so the constraint must break
		skip    string
	}{
		{name: "PowerOnVM_Task", op: powerOn, refuse: true},
		{name: "PowerOnMultiVM_Task", refuse: true, op: func(e env) error {
			res, err := methods.PowerOnMultiVM_Task(e.ctx, e.c.Client(), &types.PowerOnMultiVM_Task{
				This: e.sim.Map.getEntityDatacenter(e.vm).Self, Vm: []types.ManagedObjectReference{e.vm.Self},
			})
			if err != nil {
				return err
			}
			info, err := object.NewTask(e.c.Client(), res.Returnval).WaitForResult(e.ctx)
			if err != nil {
				return err
			}
			for _, a := range info.Result.(types.ClusterPowerOnVmResult).Attempted {
				if err = object.NewTask(e.c.Client(), *a.Task).Wait(e.ctx); err != nil {
					return err
				}
			}
			return nil
		}},
		{name: "RelocateVM_Task host only", running: true, refuse: true, op: func(e env) error {
			ref := e.dst.Self
			task, err := e.c.Relocate(e.ctx, types.VirtualMachineRelocateSpec{Host: &ref}, "")
			return wait(e, task, err)
		}},
		{name: "MigrateVM_Task", running: true, op: func(e env) error {
			task, err := e.c.Migrate(e.ctx, nil, e.host, "", "")
			return wait(e, task, err)
		}},
		// vcsim registers a VM without reading its disks from the vmx, so only the VM's own files
		// tie it to the datastore, and the fault for unreachable VM files is not settled from
		// the API reference. Skipped until it is (FU-95113-122).
		{name: "RegisterVM_Task", refuse: true, skip: "VM files fault unsettled (FU-95113-122)", op: func(e env) error {
			vmx := e.vm.Config.Files.VmPathName
			if err := e.c.Unregister(e.ctx); err != nil {
				return err
			}
			folder := object.NewFolder(e.c.Client(), *e.vm.Parent)
			pool := object.NewResourcePool(e.c.Client(), *e.vm.ResourcePool)
			task, err := folder.RegisterVM(e.ctx, vmx, e.vm.Name, false, pool, e.host)
			if err != nil {
				return err
			}
			info, err := task.WaitForResult(e.ctx)
			if err != nil {
				return err
			}
			*e.mvm = *object.NewVirtualMachine(e.c.Client(), info.Result.(types.ManagedObjectReference))
			return powerOn(env{ctx: e.ctx, c: e.mvm})
		}},
		{name: "CreateVM_Task", refuse: true, op: func(e env) error {
			spec := types.VirtualMachineConfigSpec{
				Name:    "fu122-create",
				GuestId: string(types.VirtualMachineGuestOsIdentifierOtherGuest),
				Files:   &types.VirtualMachineFileInfo{VmPathName: "[" + e.sim.Map.Get(e.vm.Datastore[0]).(*Datastore).Name + "]"},
				DeviceChange: []types.BaseVirtualDeviceConfigSpec{
					&types.VirtualDeviceConfigSpec{
						Operation: types.VirtualDeviceConfigSpecOperationAdd,
						Device: &types.VirtualLsiLogicController{VirtualSCSIController: types.VirtualSCSIController{
							VirtualController: types.VirtualController{VirtualDevice: types.VirtualDevice{Key: 1000}},
						}},
					},
					&types.VirtualDeviceConfigSpec{
						Operation:     types.VirtualDeviceConfigSpecOperationAdd,
						FileOperation: types.VirtualDeviceConfigSpecFileOperationCreate,
						Device: &types.VirtualDisk{
							VirtualDevice: types.VirtualDevice{
								Key: -1, ControllerKey: 1000, UnitNumber: types.NewInt32(0),
								Backing: &types.VirtualDiskFlatVer2BackingInfo{
									VirtualDeviceFileBackingInfo: types.VirtualDeviceFileBackingInfo{FileName: "[" + e.sim.Map.Get(e.vm.Datastore[0]).(*Datastore).Name + "]"},
								},
							},
							CapacityInKB: 1024,
						},
					},
				},
			}
			folder := object.NewFolder(e.c.Client(), *e.vm.Parent)
			pool := object.NewResourcePool(e.c.Client(), *e.vm.ResourcePool)
			task, err := folder.CreateVM(e.ctx, spec, pool, e.host)
			if err != nil {
				return err
			}
			info, err := task.WaitForResult(e.ctx)
			if err != nil {
				return err
			}
			*e.mvm = *object.NewVirtualMachine(e.c.Client(), info.Result.(types.ManagedObjectReference))
			return powerOn(env{ctx: e.ctx, c: e.mvm})
		}},
		{name: "control: power on without the check", control: true, op: func(e env) error {
			e.sim.WithLock(e.vm, func() {
				e.sim.Map.Update(e.sim, e.vm, []types.PropertyChange{{Name: "runtime.powerState", Val: types.VirtualMachinePowerStatePoweredOn}})
			})
			return nil
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.skip != "" {
				t.Skip(test.skip)
			}
			m := VPX()
			defer m.Remove()
			if err := m.Create(); err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			c := m.Service.client()
			sim := m.Service.Context
			r := m.Map()

			var vm *VirtualMachine
			for _, o := range r.All("VirtualMachine") {
				if o.(*VirtualMachine).Name == "DC0_C0_RP0_VM0" {
					vm = o.(*VirtualMachine)
				}
			}
			cvm := object.NewVirtualMachine(c, vm.Self)
			var dst *HostSystem
			for _, o := range r.All("HostSystem") {
				h := o.(*HostSystem)
				if h.Self != *vm.Runtime.Host && *h.Parent == *r.Get(*vm.Runtime.Host).(*HostSystem).Parent {
					dst = h
				}
			}
			if dst == nil {
				t.Fatal("no second host in the VM's cluster")
			}
			if !test.running {
				task, err := cvm.PowerOff(ctx)
				if err == nil {
					_ = task.Wait(ctx)
				}
				// Off on the destination host, so every method places it there.
				host := dst.Self
				sim.Map.Update(sim, vm, []types.PropertyChange{{Name: "runtime.host", Val: host}})
			}
			ds := r.Get(vm.Datastore[0]).(*Datastore)
			for i := range ds.Host {
				if ds.Host[i].Key == dst.Self {
					ds.Host[i].MountInfo.Mounted = types.NewBool(false)
					ds.Host[i].MountInfo.Accessible = types.NewBool(false)
				}
			}

			moved := new(object.VirtualMachine)
			*moved = *cvm
			err := test.op(env{ctx: ctx, vm: vm, sim: sim, c: cvm, dst: dst, mvm: moved, host: object.NewHostSystem(c, dst.Self)})
			if test.refuse {
				if _, ok := asFault(err).(*types.CannotAccessVmDisk); !ok {
					t.Errorf("fault %v, want CannotAccessVmDisk", err)
				}
			}

			running := r.Get(moved.Reference()).(*VirtualMachine)
			broken := running.Runtime.PowerState == types.VirtualMachinePowerStatePoweredOn &&
				*running.Runtime.Host == dst.Self
			if broken != test.control {
				t.Errorf("VM running on %s without its datastore %s: %t, want %t", dst.Name, ds.Name, broken, test.control)
			}
		})
	}
}

// asFault returns the method fault inside a task or SOAP error, or nil.
func asFault(err error) types.BaseMethodFault {
	if err == nil {
		return nil
	}
	if f, ok := err.(task.Error); ok {
		return f.Fault()
	}
	return nil
}
