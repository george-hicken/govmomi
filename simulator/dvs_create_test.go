// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package simulator

import (
	"context"
	"slices"
	"testing"

	"github.com/vmware/govmomi/find"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/vim25/types"
)

// TestCreateDVSWithHostMembers checks that a DVS created with host members joins them as ReconfigureDvs does: the
// DVS lists the host, the host's proxy switch names the member's uplink, and a portgroup added afterwards reaches
// the host's network list.
func TestCreateDVSWithHostMembers(t *testing.T) {
	m := VPX()
	defer m.Remove()
	if err := m.Create(); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	c := m.Service.client()
	simCtx := m.Service.Context

	finder := find.NewFinder(c, false)
	dcs, err := finder.DatacenterList(ctx, "*")
	if err != nil {
		t.Fatal(err)
	}
	finder.SetDatacenter(dcs[0])
	folders, err := dcs[0].Folders(ctx)
	if err != nil {
		t.Fatal(err)
	}
	hosts, err := finder.HostSystemList(ctx, "*/*")
	if err != nil {
		t.Fatal(err)
	}
	hostRef := hosts[0].Reference()

	task, err := folders.NetworkFolder.CreateDVS(ctx, types.DVSCreateSpec{
		ConfigSpec: &types.VMwareDVSConfigSpec{DVSConfigSpec: types.DVSConfigSpec{
			Name: "dvs-with-members",
			Host: []types.DistributedVirtualSwitchHostMemberConfigSpec{{
				Operation: string(types.ConfigSpecOperationAdd),
				Host:      hostRef,
				Backing: &types.DistributedVirtualSwitchHostMemberPnicBacking{
					PnicSpec: []types.DistributedVirtualSwitchHostMemberPnicSpec{{PnicDevice: "vmnic1"}},
				},
			}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	info, err := task.WaitForResult(ctx)
	if err != nil {
		t.Fatal(err)
	}
	dvsRef := info.Result.(types.ManagedObjectReference)
	s := simCtx.Map.Get(dvsRef).(*VmwareDistributedVirtualSwitch)

	if !slices.Equal(s.Summary.HostMember, []types.ManagedObjectReference{hostRef}) {
		t.Errorf("Summary.HostMember = %v, want [%s]", s.Summary.HostMember, hostRef)
	}
	if n := len(s.Config.GetDVSConfigInfo().Host); n != 1 {
		t.Errorf("len(Config.Host) = %d, want 1", n)
	}

	h := simCtx.Map.Get(hostRef).(*HostSystem)
	var proxySwitched bool
	for _, ps := range h.Config.Network.ProxySwitch {
		if ps.DvsUuid != s.Uuid {
			continue
		}
		proxySwitched = true
		if !slices.Contains(ps.Pnic, "key-vim.host.PhysicalNic-vmnic1") {
			t.Errorf("proxy switch pnics = %v, want vmnic1", ps.Pnic)
		}
	}
	if !proxySwitched {
		t.Errorf("host has no proxy switch for DVS %s", s.Uuid)
	}

	pgTask, err := object.NewDistributedVirtualSwitch(c, dvsRef).AddPortgroup(ctx, []types.DVPortgroupConfigSpec{{
		Name:     "pg-after-create",
		Type:     string(types.DistributedVirtualPortgroupPortgroupTypeEarlyBinding),
		NumPorts: 8,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := pgTask.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	pg := s.Portgroup[len(s.Portgroup)-1]
	if !slices.Contains(h.Network, pg) {
		t.Errorf("host network list %v lacks portgroup %s added after the join", h.Network, pg)
	}
}
