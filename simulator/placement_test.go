// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package simulator

import (
	"context"
	"fmt"
	"testing"

	"github.com/vmware/govmomi/find"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/property"
	"github.com/vmware/govmomi/vim25"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
)

// TestCreateVMPlacementFollowsPool checks that CreateVM_Task without a host places the VM on a
// host of the cluster that owns the requested resource pool, for the cluster's root pool and for a
// child pool beneath it.
func TestCreateVMPlacementFollowsPool(t *testing.T) {
	vpx := VPX()
	vpx.Cluster = 2

	Test(func(ctx context.Context, c *vim25.Client) {
		finder := find.NewFinder(c, true)
		dc, err := finder.DefaultDatacenter(ctx)
		if err != nil {
			t.Fatal(err)
		}
		finder.SetDatacenter(dc)

		clusters, err := finder.ClusterComputeResourceList(ctx, "*")
		if err != nil {
			t.Fatal(err)
		}
		if len(clusters) != vpx.Cluster {
			t.Fatalf("found %d clusters, want %d", len(clusters), vpx.Cluster)
		}

		folders, err := dc.Folders(ctx)
		if err != nil {
			t.Fatal(err)
		}

		const vmsPerPool = 10
		pc := property.DefaultCollector(c)

		for _, cluster := range clusters {
			var mcr mo.ClusterComputeResource
			if err := pc.RetrieveOne(ctx, cluster.Reference(), []string{"name", "host", "resourcePool"}, &mcr); err != nil {
				t.Fatal(err)
			}

			hosts := make(map[types.ManagedObjectReference]bool, len(mcr.Host))
			for _, h := range mcr.Host {
				hosts[h] = true
			}

			rootPool := object.NewResourcePool(c, *mcr.ResourcePool)
			childPool, err := rootPool.Create(ctx, "placement-child", types.DefaultResourceConfigSpec())
			if err != nil {
				t.Fatal(err)
			}

			pools := map[string]*object.ResourcePool{
				"root":  rootPool,
				"child": childPool,
			}

			for poolName, pool := range pools {
				for i := range vmsPerPool {
					name := fmt.Sprintf("placement-%s-%s-%d", mcr.Name, poolName, i)
					spec := types.VirtualMachineConfigSpec{
						Name:  name,
						Files: &types.VirtualMachineFileInfo{VmPathName: "[LocalDS_0] " + name},
					}

					task, err := folders.VmFolder.CreateVM(ctx, spec, pool, nil)
					if err != nil {
						t.Fatal(err)
					}
					info, err := task.WaitForResult(ctx, nil)
					if err != nil {
						t.Fatalf("%s pool %s vm %d: %v", mcr.Name, poolName, i, err)
					}

					vmRef := info.Result.(types.ManagedObjectReference)
					var mvm mo.VirtualMachine
					if err := pc.RetrieveOne(ctx, vmRef, []string{"runtime.host"}, &mvm); err != nil {
						t.Fatal(err)
					}

					if mvm.Runtime.Host == nil {
						t.Fatalf("%s pool %s vm %d: runtime.host not set", mcr.Name, poolName, i)
					}
					if !hosts[*mvm.Runtime.Host] {
						t.Errorf("%s pool %s vm %d: placed on host %s outside the cluster",
							mcr.Name, poolName, i, mvm.Runtime.Host.Value)
					}
				}
			}
		}
	}, vpx)
}
