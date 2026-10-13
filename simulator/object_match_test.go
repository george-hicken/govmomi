// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package simulator

import (
	"context"
	"os"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/property"
	"github.com/vmware/govmomi/vim25"
	"github.com/vmware/govmomi/vim25/types"
	"github.com/vmware/govmomi/vmdk"
)

const (
	idA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa1"
	idB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb2"
)

// matchTest runs f against the VPX model, with the VM DC0_H0_VM0.
func matchTest(t *testing.T, f func(ctx context.Context, c *vim25.Client, r *Registry, vm *VirtualMachine)) {
	t.Helper()
	model := VPX()
	Test(func(ctx context.Context, c *vim25.Client) {
		r := model.Map()
		f(ctx, c, r, vmNamed(t, r, "DC0_H0_VM0"))
	}, model)
}

func vmNamed(t *testing.T, r *Registry, name string) *VirtualMachine {
	t.Helper()
	for _, e := range r.All("VirtualMachine") {
		if vm := e.(*VirtualMachine); vm.Name == name {
			return vm
		}
	}
	require.FailNow(t, "no VirtualMachine named "+name)
	return nil
}

// diskPaths returns the backing file of each VirtualDisk of vm, in device order.
func diskPaths(vm *VirtualMachine) []string {
	var paths []string
	for _, dev := range vm.Config.Hardware.Device {
		if d, ok := dev.(*types.VirtualDisk); ok {
			paths = append(paths, d.Backing.(types.BaseVirtualDeviceFileBackingInfo).GetVirtualDeviceFileBackingInfo().FileName)
		}
	}
	return paths
}

// addDisk adds a 1 MiB disk to vm, through ReconfigVM_Task.
func addDisk(t *testing.T, ctx context.Context, c *vim25.Client, r *Registry, vm *VirtualMachine) {
	t.Helper()
	ovm := object.NewVirtualMachine(c, vm.Reference())
	devices, err := ovm.Device(ctx)
	require.NoError(t, err)
	controller, err := devices.FindDiskController("")
	require.NoError(t, err)
	disk := devices.CreateDisk(controller, r.Any("Datastore").(*Datastore).Reference(), "")
	disk.CapacityInBytes = 1 << 20
	require.NoError(t, ovm.AddDevice(ctx, disk))
}

// setExtraConfig sets one ExtraConfig entry on vm through ReconfigVM_Task.
func setExtraConfig(t *testing.T, ctx context.Context, c *vim25.Client, vm *VirtualMachine, key, value string) {
	t.Helper()
	spec := types.VirtualMachineConfigSpec{
		ExtraConfig: []types.BaseOptionValue{&types.OptionValue{Key: key, Value: value}},
	}
	task, err := object.NewVirtualMachine(c, vm.Reference()).Reconfigure(ctx, spec)
	require.NoError(t, err)
	require.NoError(t, task.Wait(ctx))
}

// stampDisk stamps id as the longContentID of the disk at path, as the harness does (9522.a), and returns
// the host file that holds the descriptor.
func stampDisk(t *testing.T, r *Registry, path, id string) string {
	t.Helper()
	dc := r.Any("Datacenter").(*Datacenter)
	vctx := registryContext(r)
	desc, file, fault := r.FileManager().DiskDescriptor(vctx, &dc.Self, path)
	require.Nil(t, fault)
	desc.DDB["longContentID"] = id
	n, err := strconv.ParseUint(id[len(id)-8:], 16, 32)
	require.NoError(t, err)
	desc.CID = vmdk.DiskContentID(n)
	require.Nil(t, r.FileManager().SaveDiskDescriptor(vctx, desc, file))
	return file
}

// dd is the LookasideDiskDescriptor match for the given key and value pairs.
func dd(kv ...string) LookasideMatch {
	m := property.Match{}
	for i := 0; i < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return LookasideMatch{View: LookasideDiskDescriptor, Match: m}
}

func TestObjectMatchName(t *testing.T) {
	matchTest(t, func(_ context.Context, _ *vim25.Client, r *Registry, vm *VirtualMachine) {
		assert.True(t, ObjectMatch{Include: property.Match{"name": "DC0_H0_VM*"}}.Matches(r, vm))
		assert.False(t, ObjectMatch{Include: property.Match{"name": "x*"}}.Matches(r, vm))
	})
}

func TestObjectMatchExtraConfigKey(t *testing.T) {
	matchTest(t, func(ctx context.Context, c *vim25.Client, r *Registry, vm *VirtualMachine) {
		setExtraConfig(t, ctx, c, vm, "a", "b")
		match := func(path, pattern string) bool {
			return ObjectMatch{Include: property.Match{path: pattern}}.Matches(r, vm)
		}
		assert.True(t, match(`config.extraConfig["a"]`, "b"))
		assert.True(t, match(`config.extraConfig["a"]`, "b*"))
		assert.True(t, match(`config.extraConfig["a"]`, "*"))
		assert.False(t, match(`config.extraConfig["a"]`, "c"))
		assert.False(t, match(`config.extraConfig["missing"]`, "*"))
		assert.Equal(t, "b", objectProperty(r, vm, `config.extraConfig["a"]`).Val)
	})
}

func TestObjectMatchExtraConfigEmptyValue(t *testing.T) {
	matchTest(t, func(_ context.Context, _ *vim25.Client, r *Registry, vm *VirtualMachine) {
		vm.Config.ExtraConfig = append(vm.Config.ExtraConfig, &types.OptionValue{Key: "e", Value: ""})
		m := ObjectMatch{Include: property.Match{`config.extraConfig["e"]`: "*"}}
		assert.False(t, m.Matches(r, vm))
	})
}

func TestObjectMatchExclude(t *testing.T) {
	matchTest(t, func(_ context.Context, _ *vim25.Client, r *Registry, vm *VirtualMachine) {
		include := property.Match{"name": "DC0_H0_VM0"}
		all := ObjectMatch{Include: include, Exclude: property.Match{"name": "DC0_H0_VM0"}}
		assert.False(t, all.Matches(r, vm))

		some := ObjectMatch{Include: include, Exclude: property.Match{"name": "DC0_H0_VM0", "config.template": "true"}}
		assert.True(t, some.Matches(r, vm))
	})
}

func TestObjectMatchLookasideDiskDescriptor(t *testing.T) {
	matchTest(t, func(ctx context.Context, c *vim25.Client, r *Registry, vm *VirtualMachine) {
		addDisk(t, ctx, c, r, vm)
		paths := diskPaths(vm)
		require.Len(t, paths, 2)
		stampDisk(t, r, paths[0], idA)
		stampDisk(t, r, paths[1], idB)

		records, err := r.LookasideRecords(LookasideDiskDescriptor, vm)
		require.NoError(t, err)
		require.Len(t, records, 2)
		for i, id := range []string{idA, idB} {
			assert.Equal(t, "0", recordValue(records[i], "depth"))
			assert.Equal(t, paths[i], recordValue(records[i], "file"))
			assert.Equal(t, id, recordValue(records[i], "ddb.longContentID"))
			assert.Equal(t, id[len(id)-8:], recordValue(records[i], "cid"))
		}

		on := ObjectMatch{Include: property.Match{"name": "DC0_H0_VM0"}}
		with := func(l ...LookasideMatch) ObjectMatch {
			on.Lookaside = l
			return on
		}
		assert.True(t, with(dd("ddb.longContentID", idA)).Matches(r, vm))
		assert.True(t, with(dd("ddb.longContentID", idB)).Matches(r, vm))
		assert.False(t, with(dd("ddb.longContentID", idA, "cid", "bbbbbbb2")).Matches(r, vm))
		assert.False(t, with(dd("ddb.longContentID", "c*")).Matches(r, vm))
	})
}

func TestObjectMatchLookasideViaParent(t *testing.T) {
	matchTest(t, func(ctx context.Context, c *vim25.Client, r *Registry, vm *VirtualMachine) {
		dc := object.NewDatacenter(c, r.Any("Datacenter").(*Datacenter).Reference())
		task, err := object.NewVirtualDiskManager(c).CreateVirtualDisk(ctx, "[LocalDS_0] parent.vmdk", dc,
			&types.FileBackedVirtualDiskSpec{
				VirtualDiskSpec: types.VirtualDiskSpec{AdapterType: "lsiLogic", DiskType: "thin"},
				CapacityKb:      1024,
			})
		require.NoError(t, err)
		require.NoError(t, task.Wait(ctx))
		stampDisk(t, r, "[LocalDS_0] parent.vmdk", idA)

		var disk *types.VirtualDisk
		for _, dev := range vm.Config.Hardware.Device {
			if d, ok := dev.(*types.VirtualDisk); ok {
				disk = d
				break
			}
		}
		require.NotNil(t, disk)
		backing := disk.Backing.(*types.VirtualDiskFlatVer2BackingInfo)
		backing.Parent = &types.VirtualDiskFlatVer2BackingInfo{
			VirtualDeviceFileBackingInfo: types.VirtualDeviceFileBackingInfo{FileName: "[LocalDS_0] parent.vmdk"},
		}

		records, err := r.LookasideRecords(LookasideDiskDescriptor, vm)
		require.NoError(t, err)
		require.Len(t, records, 2)
		assert.Equal(t, "0", recordValue(records[0], "depth"))
		assert.Nil(t, recordValue(records[0], "ddb.longContentID"))
		assert.Equal(t, "1", recordValue(records[1], "depth"))
		assert.Equal(t, idA, recordValue(records[1], "ddb.longContentID"))

		assert.True(t, ObjectMatch{Lookaside: []LookasideMatch{dd("ddb.longContentID", idA)}}.Matches(r, vm))
	})
}

func TestObjectMatchLookasideUnreadable(t *testing.T) {
	matchTest(t, func(_ context.Context, _ *vim25.Client, r *Registry, vm *VirtualMachine) {
		for _, dev := range vm.Config.Hardware.Device {
			if d, ok := dev.(*types.VirtualDisk); ok {
				d.Backing.(*types.VirtualDiskFlatVer2BackingInfo).FileName = "[LocalDS_0] missing.vmdk"
				break
			}
		}

		records, err := r.LookasideRecords(LookasideDiskDescriptor, vm)
		require.NoError(t, err)
		require.Len(t, records, 1)
		assert.Equal(t, "[LocalDS_0] missing.vmdk", recordValue(records[0], "file"))
		assert.Equal(t, "0", recordValue(records[0], "depth"))
		assert.NotEmpty(t, recordValue(records[0], "error"))

		m := ObjectMatch{Lookaside: []LookasideMatch{dd("ddb.longContentID", "*")}}
		assert.False(t, m.Matches(r, vm))
	})
}

func TestObjectMatchVcsimPathAbsent(t *testing.T) {
	matchTest(t, func(_ context.Context, _ *vim25.Client, r *Registry, vm *VirtualMachine) {
		stampDisk(t, r, diskPaths(vm)[0], idA)

		m := ObjectMatch{Include: property.Match{"vcsim.disk.origin.itemID": "*"}}
		assert.False(t, m.Matches(r, vm))
		assert.Nil(t, objectProperty(r, vm, "vcsim.disk.origin.itemID").Val)
	})
}

func TestLookasideRecordsUnknownView(t *testing.T) {
	matchTest(t, func(_ context.Context, _ *vim25.Client, r *Registry, vm *VirtualMachine) {
		_, err := r.LookasideRecords("nope", vm)
		assert.Error(t, err)

		_, err = r.LookasideRecords(LookasideDiskDescriptor, r.Any("HostSystem"))
		assert.Error(t, err)

		m := ObjectMatch{
			Include: property.Match{"name": "*"},
			Lookaside: []LookasideMatch{
				{View: "nope", Match: property.Match{"ddb.uuid": "*"}},
			},
		}
		assert.False(t, m.Matches(r, vm))
		results := m.Explain(r, vm)
		require.Len(t, results, 2)
		assert.Nil(t, results[1].Value)
		assert.False(t, results[1].Matched)
	})
}

func TestSaveDiskDescriptorAtomic(t *testing.T) {
	matchTest(t, func(_ context.Context, _ *vim25.Client, r *Registry, vm *VirtualMachine) {
		file := stampDisk(t, r, diskPaths(vm)[0], idA)

		_, err := os.Stat(file + ".tmp")
		assert.True(t, os.IsNotExist(err))

		dc := r.Any("Datacenter").(*Datacenter)
		desc, _, fault := r.FileManager().DiskDescriptor(registryContext(r), &dc.Self, diskPaths(vm)[0])
		require.Nil(t, fault)
		assert.Equal(t, idA, desc.DDB["longContentID"])
	})
}

func TestObjectMatchExplain(t *testing.T) {
	matchTest(t, func(ctx context.Context, c *vim25.Client, r *Registry, vm *VirtualMachine) {
		addDisk(t, ctx, c, r, vm)
		paths := diskPaths(vm)
		require.Len(t, paths, 2)
		stampDisk(t, r, paths[0], idA)

		m := ObjectMatch{
			Include:   property.Match{"name": "DC0_H0_VM0", "runtime.powerState": "poweredOn"},
			Lookaside: []LookasideMatch{dd("ddb.longContentID", "c*")},
			Exclude:   property.Match{"config.template": "true"},
		}
		assert.False(t, m.Matches(r, vm))

		results := m.Explain(r, vm)
		require.Len(t, results, 4)
		assert.Equal(t, "name", results[0].Key)
		assert.Equal(t, "runtime.powerState", results[1].Key)
		assert.Equal(t, "lookaside:diskDescriptor", results[2].Key)
		assert.Equal(t, "ddb.longContentID=c*", results[2].Want)
		assert.True(t, results[3].Exclude)

		results = m.Explain(r, vm)
		assert.Equal(t, types.ArrayOfString{String: []string{
			"ddb.longContentID=" + idA,
			"ddb.longContentID=<absent>",
		}}, results[2].Value)

		stampDisk(t, r, paths[1], idB)
		results = m.Explain(r, vm)
		assert.Equal(t, types.ArrayOfString{String: []string{
			"ddb.longContentID=" + idA,
			"ddb.longContentID=" + idB,
		}}, results[2].Value)
	})
}

func TestObjectMatchFaultFilter(t *testing.T) {
	matchTest(t, func(ctx context.Context, c *vim25.Client, r *Registry, vm0 *VirtualMachine) {
		vm1 := vmNamed(t, r, "DC0_H0_VM1")
		for _, vm := range []*VirtualMachine{vm0, vm1} {
			vm.Runtime.PowerState = types.VirtualMachinePowerStatePoweredOff
		}
		stampDisk(t, r, diskPaths(vm0)[0], idA)

		service := ServiceFromContext(ctx)
		service.AddFaultRule(&FaultInjectionRule{
			MethodName:  "PowerOnVM_Task",
			ObjectType:  "*",
			ObjectName:  "*",
			Probability: 1.0,
			FaultType:   FaultTypeGeneric,
			Message:     "power on refused",
			Enabled:     true,
			InclusionPropertyFilter: ObjectMatch{
				Include:   property.Match{"name": "*"},
				Lookaside: []LookasideMatch{dd("ddb.longContentID", idA)},
			}.FaultFilter(r),
		})

		_, err := object.NewVirtualMachine(c, vm0.Reference()).PowerOn(ctx)
		assert.Error(t, err)

		task, err := object.NewVirtualMachine(c, vm1.Reference()).PowerOn(ctx)
		require.NoError(t, err)
		assert.NoError(t, task.Wait(ctx))
	})
}
