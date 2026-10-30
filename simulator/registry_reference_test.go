// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package simulator

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/vmware/govmomi/find"
	"github.com/vmware/govmomi/property"
	"github.com/vmware/govmomi/vim25"
	"github.com/vmware/govmomi/vim25/methods"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
)

// TestDestroyVMUpdatesHostVm checks a watcher on HostSystem.vm sees a destroyed VM leave it.
// Registry.RemoveReference used to mutate host.Vm without a property change, so the
// watcher's view kept the VM forever.
func TestDestroyVMUpdatesHostVm(t *testing.T) {
	Test(func(ctx context.Context, c *vim25.Client) {
		vm, err := find.NewFinder(c).VirtualMachine(ctx, "DC0_H0_VM0")
		if err != nil {
			t.Fatal(err)
		}
		host, err := vm.HostSystem(ctx)
		if err != nil {
			t.Fatal(err)
		}

		pc := property.DefaultCollector(c)
		ready := make(chan struct{})
		gone := make(chan error, 1)
		wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()

		go func() {
			first := true
			gone <- property.Wait(wctx, pc, host.Reference(), []string{"vm"}, func(changes []types.PropertyChange) bool {
				if first {
					first = false
					close(ready)
					return false
				}
				for _, change := range changes {
					refs := change.Val.(types.ArrayOfManagedObjectReference).ManagedObjectReference
					if !slices.Contains(refs, vm.Reference()) {
						return true
					}
				}
				return false
			})
		}()
		<-ready

		task, err := vm.PowerOff(ctx)
		if err == nil {
			_ = task.Wait(ctx)
		}
		task, err = vm.Destroy(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err = task.Wait(ctx); err != nil {
			t.Fatal(err)
		}

		if err = <-gone; err != nil {
			t.Fatalf("watcher never saw %s leave %s.vm: %v", vm.Reference(), host.Reference(), err)
		}
	})
}

// TestRegistryReferenceMutatorsUpdate is the class check for FU-95113-125: a registry
// mutation of a managed-object property with no update event. Each row mutates a registered
// object server-side and asserts through a real WaitForUpdatesEx collector, which is what
// product clients see. The rows cover every reference-list mutator (AppendReference,
// AddReference, RemoveReference, removeString, all via updateField), nested fields, and the
// ContainerView membership diff. The vSphere API reference defines the update kinds the rows
// expect (vmodl.query.PropertyCollector.ObjectUpdate.Kind,
// https://developer.broadcom.com/xapis/vsphere-web-services-api/latest/vmodl.query.PropertyCollector.ObjectUpdate.Kind.html):
//
//   - enter: "A managed object became visible to a filter for the first time."
//   - leave: "A managed object left the set of objects visible to a filter."
//   - modify: "A property of the managed object changed its value."
//
// The control row mutates a field without updateField; the collector must see nothing, which
// proves the table detects a mutator that skips the update.
func TestRegistryReferenceMutatorsUpdate(t *testing.T) {
	m := VPX()
	defer m.Remove()
	if err := m.Create(); err != nil {
		t.Fatal(err)
	}
	c := m.Service.client()
	ctx := context.Background()
	sim := m.Service.Context
	r := m.Map()

	ref := types.ManagedObjectReference{Type: "VirtualMachine", Value: "vm-fu125"}
	host := r.Any("HostSystem").(*HostSystem)
	dvs := r.Any("VmwareDistributedVirtualSwitch").(*VmwareDistributedVirtualSwitch)
	vm := r.Any("VirtualMachine").(*VirtualMachine)
	root := r.Get(c.ServiceContent.RootFolder).(*Folder)

	// prop watches one property of obj.
	prop := func(obj mo.Reference, path string) types.PropertyFilterSpec {
		return types.PropertyFilterSpec{
			ObjectSet: []types.ObjectSpec{{Obj: obj.Reference()}},
			PropSet:   []types.PropertySpec{{Type: obj.Reference().Type, PathSet: []string{path}}},
		}
	}
	// modified reports whether u modifies path of obj.
	modified := func(obj mo.Reference, path string) func([]types.ObjectUpdate) bool {
		return func(updates []types.ObjectUpdate) bool {
			for _, u := range updates {
				if u.Obj == obj.Reference() && u.Kind == types.ObjectUpdateKindModify &&
					slices.ContainsFunc(u.ChangeSet, func(c types.PropertyChange) bool { return c.Name == path }) {
					return true
				}
			}
			return false
		}
	}
	// membership watches the folders of a recursive ContainerView on the root folder.
	var view types.ManagedObjectReference
	membership := func() types.PropertyFilterSpec {
		res, err := methods.CreateContainerView(ctx, c, &types.CreateContainerView{
			This: *c.ServiceContent.ViewManager, Container: root.Self, Type: []string{"Folder"}, Recursive: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		view = res.Returnval
		return types.PropertyFilterSpec{
			ObjectSet: []types.ObjectSpec{{Obj: view, Skip: types.NewBool(true), SelectSet: []types.BaseSelectionSpec{
				&types.TraversalSpec{Type: "ContainerView", Path: "view"},
			}}},
			PropSet: []types.PropertySpec{{Type: "Folder", PathSet: []string{"name"}}},
		}
	}
	var joined types.ManagedObjectReference
	member := func(kind types.ObjectUpdateKind) func([]types.ObjectUpdate) bool {
		return func(updates []types.ObjectUpdate) bool {
			return slices.ContainsFunc(updates, func(u types.ObjectUpdate) bool { return u.Kind == kind && u.Obj == joined })
		}
	}

	tests := []struct {
		name   string
		spec   func() types.PropertyFilterSpec
		mutate func()
		seen   func([]types.ObjectUpdate) bool
		want   bool
	}{
		{"AppendReference", func() types.PropertyFilterSpec { return prop(host, "vm") },
			func() { r.AppendReference(sim, host, &host.Vm, ref) }, modified(host, "vm"), true},
		{"AddReference", func() types.PropertyFilterSpec { return prop(host, "datastore") },
			func() { r.AddReference(sim, host, &host.Datastore, ref) }, modified(host, "datastore"), true},
		{"RemoveReference", func() types.PropertyFilterSpec { return prop(host, "vm") },
			func() { r.RemoveReference(sim, host, &host.Vm, ref) }, modified(host, "vm"), true},
		{"AddReference nested", func() types.PropertyFilterSpec { return prop(dvs, "summary.vm") },
			func() { r.AddReference(sim, dvs, &dvs.Summary.Vm, ref) }, modified(dvs, "summary.vm"), true},
		{"RemoveReference nested", func() types.PropertyFilterSpec { return prop(dvs, "summary.vm") },
			func() { r.RemoveReference(sim, dvs, &dvs.Summary.Vm, ref) }, modified(dvs, "summary.vm"), true},
		{"removeString", func() types.PropertyFilterSpec { return prop(dvs, "summary.portgroupName") },
			func() { r.removeString(sim, dvs, &dvs.Summary.PortgroupName, dvs.Summary.PortgroupName[0]) },
			modified(dvs, "summary.portgroupName"), true},
		{"ContainerView enter", membership,
			func() {
				f := &Folder{}
				f.Name = "fu125"
				folderPutChild(sim, &root.Folder, f)
				joined = f.Self
			},
			member(types.ObjectUpdateKindEnter), true},
		{"ContainerView leave", membership,
			func() { folderRemoveChild(sim, &root.Folder, joined) },
			member(types.ObjectUpdateKindLeave), true},
		// Control: the mutation bypasses updateField, so no client sees it.
		{"control: bypasses updateField", func() types.PropertyFilterSpec { return prop(vm, "datastore") },
			func() { r.WithLock(sim, vm, func() { vm.Datastore = append(vm.Datastore, ref) }) },
			modified(vm, "datastore"), false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := observe(ctx, t, c, test.spec(), test.mutate, test.seen); got != test.want {
				t.Errorf("collector saw the change: %t, want %t", got, test.want)
			}
		})
	}
}

// observe creates a property collector with a filter for spec, drains the initial update, runs
// mutate and reports whether the next WaitForUpdatesEx call returns an update matching seen.
func observe(ctx context.Context, t *testing.T, c *vim25.Client, spec types.PropertyFilterSpec, mutate func(), seen func([]types.ObjectUpdate) bool) bool {
	t.Helper()
	pc, err := property.DefaultCollector(c).Create(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pc.Destroy(ctx) }()
	if _, err = pc.CreateFilter(ctx, types.CreateFilter{Spec: spec}); err != nil {
		t.Fatal(err)
	}
	wait := func(version string) *types.UpdateSet {
		res, err := methods.WaitForUpdatesEx(ctx, c, &types.WaitForUpdatesEx{
			This: pc.Reference(), Version: version, Options: &types.WaitOptions{MaxWaitSeconds: types.NewInt32(1)},
		})
		if err != nil {
			t.Fatal(err)
		}
		return res.Returnval
	}
	initial := wait("")
	if initial == nil {
		t.Fatal("no initial update")
	}
	mutate()
	set := wait(initial.Version)
	if set == nil {
		return false
	}
	var updates []types.ObjectUpdate
	for _, fs := range set.FilterSet {
		updates = append(updates, fs.ObjectSet...)
	}
	return seen(updates)
}
