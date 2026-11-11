// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package simulator

import (
	"context"
	"fmt"
	"testing"

	"github.com/vmware/govmomi/cns"
	cnstypes "github.com/vmware/govmomi/cns/types"
	"github.com/vmware/govmomi/find"
	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/vim25"
	vim25types "github.com/vmware/govmomi/vim25/types"
)

// createErr creates one volume from spec and returns why it failed: the task error, or the
// operation's fault. A nil result means the volume was created.
func createErr(ctx context.Context, t *testing.T, c *cns.Client, spec cnstypes.CnsVolumeCreateSpec) error {
	t.Helper()
	task, err := c.CreateVolume(ctx, []cnstypes.CnsVolumeCreateSpec{spec})
	if err != nil {
		t.Fatal(err)
	}
	info, err := cns.GetTaskInfo(ctx, task)
	if err != nil {
		return err
	}
	res, err := cns.GetTaskResult(ctx, info)
	if err != nil {
		return err
	}
	if f := res.GetCnsVolumeOperationResult().Fault; f != nil {
		return fmt.Errorf("%s", f.LocalizedMessage)
	}
	return nil
}

// TestCreateVolumePlacesOnActiveClusters: with no Datastores, CNS places the volume on a datastore
// the ActiveClusters reach, as vsphere-csi expects when a zone's clusters share no datastore; with
// neither datastores, clusters nor hosts the request is still invalid.
func TestCreateVolumePlacesOnActiveClusters(t *testing.T) {
	simulator.Test(func(ctx context.Context, vc *vim25.Client) {
		c, err := cns.NewClient(ctx, vc)
		if err != nil {
			t.Fatal(err)
		}
		cluster, err := find.NewFinder(vc).DefaultClusterComputeResource(ctx)
		if err != nil {
			t.Fatal(err)
		}
		spec := func(name string) cnstypes.CnsVolumeCreateSpec {
			return cnstypes.CnsVolumeCreateSpec{
				Name:       name,
				VolumeType: string(cnstypes.CnsVolumeTypeBlock),
				BackingObjectDetails: &cnstypes.CnsBlockBackingDetails{
					CnsBackingObjectDetails: cnstypes.CnsBackingObjectDetails{CapacityInMb: 1024},
				},
			}
		}

		byCluster := spec("by-cluster")
		byCluster.ActiveClusters = []vim25types.ManagedObjectReference{cluster.Reference()}
		if err := createErr(ctx, t, c, byCluster); err != nil {
			t.Fatalf("create with ActiveClusters only: %v", err)
		}
		if err := createErr(ctx, t, c, spec("nowhere")); err == nil {
			t.Fatal("create with no datastores, clusters or hosts: want an error, got none")
		}
	})
}
