// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package simulator

import (
	"context"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vmware/govmomi/find"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/property"
	"github.com/vmware/govmomi/view"
	"github.com/vmware/govmomi/vim25"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
)

// TestContainerViewFilterReportsOnlyMembershipChange checks a ContainerView filter reports a
// view change as vCenter does: an enter for the object that joined, a leave for the one that
// left, and nothing for the rest. Before, any view change made the filter re-enter every object
// in the view.
func TestContainerViewFilterReportsOnlyMembershipChange(t *testing.T) {
	Test(func(ctx context.Context, c *vim25.Client) {
		v, err := view.NewManager(c).CreateContainerView(ctx, c.ServiceContent.RootFolder, []string{"Folder", "VirtualMachine", "HostSystem"}, true)
		if err != nil {
			t.Fatal(err)
		}
		pc := property.DefaultCollector(c)
		filter := new(property.WaitFilter)
		filter.Add(v.Reference(), "Folder", []string{"name"}, v.TraversalSpec())
		filter.Spec.PropSet = append(filter.Spec.PropSet,
			types.PropertySpec{Type: "VirtualMachine", PathSet: []string{"name"}},
			types.PropertySpec{Type: "HostSystem", PathSet: []string{"vm"}})

		updates := make(chan []types.ObjectUpdate, 16)
		done := make(chan error, 1)
		var stop atomic.Bool
		go func() {
			err := property.WaitForUpdates(ctx, pc, filter, func(u []types.ObjectUpdate) bool {
				updates <- u
				return stop.Load()
			})
			done <- err
		}()
		<-updates // initial enter of the whole view

		// next gathers the updates of kind for the next 2s, past the collector's batch window.
		next := func(kind types.ObjectUpdateKind) []types.ManagedObjectReference {
			var refs []types.ManagedObjectReference
			timeout := time.After(2 * time.Second)
			for {
				select {
				case u := <-updates:
					for _, ou := range u {
						if ou.Kind == kind {
							refs = append(refs, ou.Obj)
						}
					}
				case <-timeout:
					return refs
				}
			}
		}

		folder, err := object.NewRootFolder(c).CreateFolder(ctx, "joined")
		if err != nil {
			t.Fatal(err)
		}
		if got := next(types.ObjectUpdateKindEnter); len(got) != 1 || got[0] != folder.Reference() {
			t.Errorf("create folder: enter %v, want only %s", got, folder.Reference())
		}

		vm, err := find.NewFinder(c).VirtualMachine(ctx, "DC0_H0_VM0")
		if err != nil {
			t.Fatal(err)
		}
		task, err := vm.PowerOff(ctx)
		if err == nil {
			_ = task.Wait(ctx)
		}
		_ = next(types.ObjectUpdateKindModify)
		task, err = vm.Destroy(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err = task.Wait(ctx); err != nil {
			t.Fatal(err)
		}

		var leave, enter []types.ManagedObjectReference
		timeout := time.After(2 * time.Second)
	collect:
		for {
			select {
			case u := <-updates:
				for _, ou := range u {
					switch ou.Kind {
					case types.ObjectUpdateKindLeave:
						leave = append(leave, ou.Obj)
					case types.ObjectUpdateKindEnter:
						enter = append(enter, ou.Obj)
					}
				}
			case <-timeout:
				break collect
			}
		}
		if len(leave) != 1 || leave[0] != vm.Reference() {
			t.Errorf("destroy vm: leave %v, want only %s", leave, vm.Reference())
		}
		if len(enter) != 0 {
			t.Errorf("destroy vm: enter %v, want none", enter)
		}

		// The host's own state drops the VM too (run182: host-38 kept vm-86).
		var hosts []mo.HostSystem
		if err = v.Retrieve(ctx, []string{"HostSystem"}, []string{"vm"}, &hosts); err != nil {
			t.Fatal(err)
		}
		for _, h := range hosts {
			if slices.Contains(h.Vm, vm.Reference()) {
				t.Errorf("%s.vm still lists %s", h.Self, vm.Reference())
			}
		}

		// End the waiter with one more update, so the server holds no open wait at close.
		stop.Store(true)
		if _, err = folder.Rename(ctx, "stop"); err != nil {
			t.Fatal(err)
		}
		if err = <-done; err != nil {
			t.Fatal(err)
		}
	})
}
