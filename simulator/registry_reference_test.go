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

// TestRegistryReferenceMutatorsUpdate covers every Registry.*Reference mutator: each must
// publish the changed field through the property-update path, nested fields included.
func TestRegistryReferenceMutatorsUpdate(t *testing.T) {
	ctx := NewContext()
	r := ctx.Map
	ref := types.ManagedObjectReference{Type: "VirtualMachine", Value: "vm-1"}

	dvs := &mo.DistributedVirtualSwitch{}
	dvs.Self = types.ManagedObjectReference{Type: "DistributedVirtualSwitch", Value: "dvs-1"}
	host := &mo.HostSystem{}
	host.Self = types.ManagedObjectReference{Type: "HostSystem", Value: "host-1"}

	tests := []struct {
		name   string
		obj    mo.Reference
		path   string
		mutate func()
	}{
		{"AppendReference", host, "vm", func() { r.AppendReference(ctx, host, &host.Vm, ref) }},
		{"AddReference", host, "datastore", func() { r.AddReference(ctx, host, &host.Datastore, ref) }},
		{"RemoveReference", host, "vm", func() { r.RemoveReference(ctx, host, &host.Vm, ref) }},
		{"AddReference nested", dvs, "summary.vm", func() { r.AddReference(ctx, dvs, &dvs.Summary.Vm, ref) }},
		{"RemoveReference nested", dvs, "summary.vm", func() { r.RemoveReference(ctx, dvs, &dvs.Summary.Vm, ref) }},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rec := &updateRecorder{}
			r.AddHandler(rec)
			defer r.RemoveHandler(rec)

			test.mutate()

			if !slices.Contains(rec.paths, test.path) {
				t.Errorf("%s: no update for %q, got %v", test.name, test.path, rec.paths)
			}
		})
	}
}

type updateRecorder struct {
	paths []string
}

func (u *updateRecorder) Reference() types.ManagedObjectReference {
	return types.ManagedObjectReference{Type: "updateRecorder", Value: "rec"}
}

func (u *updateRecorder) PutObject(*Context, mo.Reference)                    {}
func (u *updateRecorder) RemoveObject(*Context, types.ManagedObjectReference) {}

func (u *updateRecorder) UpdateObject(_ *Context, _ mo.Reference, changes []types.PropertyChange) {
	for _, change := range changes {
		u.paths = append(u.paths, change.Name)
	}
}
