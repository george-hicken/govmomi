// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package simulator_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/vmware/govmomi/find"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/property"
	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/task"
	"github.com/vmware/govmomi/vapi/library"
	"github.com/vmware/govmomi/vapi/rest"
	"github.com/vmware/govmomi/vapi/vcenter"
	"github.com/vmware/govmomi/vim25"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/soap"
	"github.com/vmware/govmomi/vim25/types"
	"github.com/vmware/govmomi/vmdk"
)

const (
	contentIDV1    = "0123456789abcdef0123456789abcd01"
	contentIDV2    = "0123456789abcdef0123456789abcd02"
	contentIDItem2 = "0123456789abcdef0123456789abcd03"

	scopeKey       = "example.scope"
	scopeValue     = "pool-a"
	ruleLookaside  = "lookaside:" + simulator.LookasideDiskDescriptor
	itemDiskName   = "image-disk1.vmdk"
	containerKey   = "RUN.container"
	requireRule    = "require-container"
	byNameRule     = "by-name"
	contentV1Rule  = "content-v1"
	contentV2Rule  = "content-v2"
	contentAnyRule = "content-any"
)

// contentIDDisk is a 1 MiB stream-optimized disk whose descriptor carries id as ddb.longContentID and
// whose CID is the last 8 hex digits of id.
func contentIDDisk(t *testing.T, id string) []byte {
	t.Helper()
	cid, err := strconv.ParseUint(id[len(id)-8:], 16, 32)
	if err != nil {
		t.Fatal(err)
	}
	name := filepath.Join(t.TempDir(), "disk.vmdk")
	w, err := vmdk.NewStreamOptimizedWriter(name, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	w.CID = vmdk.DiskContentID(cid)
	w.DDB = map[string]string{"longContentID": id}
	if err = w.Write(strings.NewReader("")); err != nil {
		t.Fatal(err)
	}
	if err = w.Close(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

type itemFile struct {
	name string
	data []byte
}

// uploadItemFiles adds files to one update session of the library item and completes it.
func uploadItemFiles(ctx context.Context, t *testing.T, m *library.Manager, itemID string, files ...itemFile) {
	t.Helper()
	session, err := m.CreateLibraryItemUpdateSession(ctx, library.Session{LibraryItemID: itemID})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		update, err := m.AddLibraryItemFile(ctx, session, library.UpdateFile{
			Name:       f.name,
			SourceType: "PUSH",
			Size:       int64(len(f.data)),
		})
		if err != nil {
			t.Fatal(err)
		}
		u, err := url.Parse(update.UploadEndpoint.URI)
		if err != nil {
			t.Fatal(err)
		}
		p := soap.DefaultUpload
		p.ContentLength = int64(len(f.data))
		if err = m.Client.Upload(ctx, bytes.NewReader(f.data), u, &p); err != nil {
			t.Fatal(err)
		}
	}
	if err = m.CompleteLibraryItemUpdateSession(ctx, session); err != nil {
		t.Fatal(err)
	}
}

// diskVMSpec is a VM with one existing disk, dir/disk-0.vmdk, on a paravirtual SCSI controller. A
// non-empty scope sets scopeKey, which the content rules require.
func diskVMSpec(name, dir, scope string) types.VirtualMachineConfigSpec {
	spec := types.VirtualMachineConfigSpec{
		Name:     name,
		GuestId:  string(types.VirtualMachineGuestOsIdentifierOtherLinux64Guest),
		NumCPUs:  1,
		MemoryMB: 512,
		Files:    &types.VirtualMachineFileInfo{VmPathName: fmt.Sprintf("[LocalDS_0] %s/%s.vmx", dir, name)},
		DeviceChange: []types.BaseVirtualDeviceConfigSpec{
			&types.VirtualDeviceConfigSpec{
				Operation: types.VirtualDeviceConfigSpecOperationAdd,
				Device:    &types.ParaVirtualSCSIController{VirtualSCSIController: types.VirtualSCSIController{VirtualController: types.VirtualController{VirtualDevice: types.VirtualDevice{Key: 100}}}},
			},
			&types.VirtualDeviceConfigSpec{
				Operation: types.VirtualDeviceConfigSpecOperationAdd,
				Device: &types.VirtualDisk{
					VirtualDevice: types.VirtualDevice{
						Key:           -1,
						ControllerKey: 100,
						UnitNumber:    types.NewInt32(0),
						Backing: &types.VirtualDiskFlatVer2BackingInfo{
							VirtualDeviceFileBackingInfo: types.VirtualDeviceFileBackingInfo{FileName: fmt.Sprintf("[LocalDS_0] %s/disk-0.vmdk", dir)},
							DiskMode:                     string(types.VirtualDiskModePersistent),
							ThinProvisioned:              types.NewBool(true),
						},
					},
					CapacityInKB: 1024,
				},
			},
		},
	}
	if scope != "" {
		spec.ExtraConfig = []types.BaseOptionValue{&types.OptionValue{Key: scopeKey, Value: scope}}
	}
	return spec
}

// extraConfigValue reads one ExtraConfig value of vm, or "" when absent.
func extraConfigValue(ctx context.Context, t *testing.T, c *vim25.Client, vm types.ManagedObjectReference, key string) string {
	t.Helper()
	var mvm mo.VirtualMachine
	if err := property.DefaultCollector(c).RetrieveOne(ctx, vm, []string{"config.extraConfig"}, &mvm); err != nil {
		t.Fatal(err)
	}
	for _, o := range mvm.Config.ExtraConfig {
		if ov := o.GetOptionValue(); ov.Key == key {
			return fmt.Sprint(ov.Value)
		}
	}
	return ""
}

// copyThinDisk copies src to dst as a thin lsiLogic disk without force.
func copyThinDisk(ctx context.Context, c *vim25.Client, dc *object.Datacenter, src, dst string) error {
	vdm := object.NewVirtualDiskManager(c)
	destSpec := &types.FileBackedVirtualDiskSpec{VirtualDiskSpec: types.VirtualDiskSpec{AdapterType: "lsiLogic", DiskType: "thin"}}
	tk, err := vdm.CopyVirtualDisk(ctx, src, dc, dst, dc, destSpec, false)
	if err != nil {
		return err
	}
	return tk.Wait(ctx)
}

// recordProp reads one property of a lookaside record, or "" when the record lacks it.
func recordProp(rec simulator.LookasideRecord, name string) string {
	for _, p := range rec {
		if p.Name == name {
			return fmt.Sprint(p.Val)
		}
	}
	return ""
}

// TestVMRulesDiskContentID checks that a disk's content ID reaches VMRules through the paths a VM's
// disk takes from a content library: upload, CopyVirtualDisk (twice, as an image cache and then a
// per-VM copy), and CreateVM. It also checks the cases with no content ID: an empty disk created by
// CreateVirtualDisk, which a power-on rule must fault, and a disk deployed by DeployLibraryItem. A
// second upload session that replaces the item's disk must not change VMs already created from it.
func TestVMRulesDiskContentID(t *testing.T) {
	model := simulator.VPX()
	simulator.Test(func(ctx context.Context, c *vim25.Client) {
		rc := rest.NewClient(c)
		if err := rc.Login(ctx, simulator.DefaultLogin); err != nil {
			t.Fatal(err)
		}
		finder := find.NewFinder(c)
		dc, err := finder.DefaultDatacenter(ctx)
		if err != nil {
			t.Fatal(err)
		}
		finder.SetDatacenter(dc)
		ds, err := finder.Datastore(ctx, "LocalDS_0")
		if err != nil {
			t.Fatal(err)
		}
		pool, err := finder.ResourcePool(ctx, "DC0_C0/Resources")
		if err != nil {
			t.Fatal(err)
		}
		dvpg, err := finder.Network(ctx, "DC0_DVPG0")
		if err != nil {
			t.Fatal(err)
		}
		folder, err := finder.DefaultFolder(ctx)
		if err != nil {
			t.Fatal(err)
		}

		m := library.NewManager(rc)
		libID, err := m.CreateLibrary(ctx, library.Library{
			Name:    "content-id-lib",
			Type:    "LOCAL",
			Storage: []library.StorageBacking{{DatastoreID: ds.Reference().Value, Type: "DATASTORE"}},
		})
		if err != nil {
			t.Fatal(err)
		}
		itemPath := func(itemID, file string) string {
			return fmt.Sprintf("[LocalDS_0] contentlib-%s/%s/%s", libID, itemID, file)
		}

		// An OVF item whose disk carries content ID v1, and a disk-only item with its own ID.
		i1, err := m.CreateLibraryItem(ctx, library.Item{Name: "image", Type: library.ItemTypeOVF, LibraryID: libID})
		if err != nil {
			t.Fatal(err)
		}
		ovf, err := os.ReadFile("../../ovf/fixtures/ttylinux.ovf")
		if err != nil {
			t.Fatal(err)
		}
		desc := strings.ReplaceAll(string(ovf), "ttylinux-pc_i486-16.1-disk1.vmdk", itemDiskName)
		uploadItemFiles(ctx, t, m, i1,
			itemFile{"image.ovf", []byte(desc)},
			itemFile{itemDiskName, contentIDDisk(t, contentIDV1)})
		i2, err := m.CreateLibraryItem(ctx, library.Item{Name: "item-2", Type: library.ItemTypeOVF, LibraryID: libID})
		if err != nil {
			t.Fatal(err)
		}
		uploadItemFiles(ctx, t, m, i2, itemFile{"item-2-disk1.vmdk", contentIDDisk(t, contentIDItem2)})

		scoped := func() property.Match {
			return property.Match{fmt.Sprintf("config.extraConfig[%q]", scopeKey): scopeValue}
		}
		patched := func() property.Match { return property.Match{fmt.Sprintf("config.extraConfig[%q]", containerKey): "*"} }
		byContentID := func(id string) []simulator.LookasideMatch {
			return []simulator.LookasideMatch{{View: simulator.LookasideDiskDescriptor, Match: property.Match{"ddb.longContentID": id}}}
		}
		patch := func(image string) *types.VirtualMachineConfigSpec {
			return &types.VirtualMachineConfigSpec{ExtraConfig: []types.BaseOptionValue{
				&types.OptionValue{Key: containerKey, Value: image}}}
		}
		for _, r := range []simulator.VMRule{
			{Name: byNameRule, Phases: []simulator.VMRulePhase{simulator.VMRulePhaseCreate},
				Match: simulator.ObjectMatch{Include: property.Match{"name": "unscoped-vm"}, Exclude: patched()},
				Patch: patch("image:by-name")},
			{Name: contentV1Rule, Match: simulator.ObjectMatch{Include: scoped(), Lookaside: byContentID(contentIDV1), Exclude: patched()},
				Patch: patch("image:v1")},
			{Name: contentV2Rule, Match: simulator.ObjectMatch{Include: scoped(), Lookaside: byContentID(contentIDV2), Exclude: patched()},
				Patch: patch("image:v2")},
			{Name: contentAnyRule, Match: simulator.ObjectMatch{Include: scoped(), Lookaside: byContentID("*"), Exclude: patched()},
				Patch: patch("image:default")},
			{Name: requireRule, Phases: []simulator.VMRulePhase{simulator.VMRulePhasePowerOn},
				Match: simulator.ObjectMatch{Include: scoped(), Exclude: patched()},
				Fault: "VM has no container config: no content rule matched a disk content ID"},
		} {
			if err := model.Map().VMRules.Add(r); err != nil {
				t.Fatal(err)
			}
		}

		mkdir := func(dir string) {
			t.Helper()
			if err := object.NewFileManager(c).MakeDirectory(ctx, "[LocalDS_0] "+dir, dc, true); err != nil {
				t.Fatal(err)
			}
		}
		// stage copies src into a new directory dir as its disk-0.vmdk.
		stage := func(src, dir string) {
			t.Helper()
			mkdir(dir)
			if err := copyThinDisk(ctx, c, dc, src, "[LocalDS_0] "+dir+"/disk-0.vmdk"); err != nil {
				t.Fatal(err)
			}
		}
		create := func(name, dir, scope string) *object.VirtualMachine {
			t.Helper()
			tk, err := folder.CreateVM(ctx, diskVMSpec(name, dir, scope), pool, nil)
			if err != nil {
				t.Fatal(err)
			}
			info, err := tk.WaitForResult(ctx)
			if err != nil {
				t.Fatal(err)
			}
			return object.NewVirtualMachine(c, info.Result.(types.ManagedObjectReference))
		}
		records := func(vm *object.VirtualMachine) []simulator.LookasideRecord {
			t.Helper()
			recs, err := model.Map().LookasideRecords(simulator.LookasideDiskDescriptor, model.Map().Get(vm.Reference()))
			if err != nil {
				t.Fatal(err)
			}
			return recs
		}
		// lookaside returns rule's lookaside result in the power-on explanation of the VM name.
		lookaside := func(name, rule string) simulator.MatchResult {
			t.Helper()
			exps, err := model.Map().VMRules.ExplainVM(model.Map(), name, simulator.VMRulePhasePowerOn)
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range exps {
				if e.Rule != rule {
					continue
				}
				for _, r := range e.Results {
					if r.Key == ruleLookaside {
						return r
					}
				}
			}
			t.Fatalf("no %s result for rule %s", ruleLookaside, rule)
			return simulator.MatchResult{}
		}

		// The content ID survives upload, the cache copy and the per-VM copy, and selects its rule.
		mkdir("cache-v1")
		if err := copyThinDisk(ctx, c, dc, itemPath(i1, itemDiskName), "[LocalDS_0] cache-v1/disk.vmdk"); err != nil {
			t.Fatal(err)
		}
		stage("[LocalDS_0] cache-v1/disk.vmdk", "vm1")
		vm1 := create("vm1", "vm1", scopeValue)
		recs := records(vm1)
		if len(recs) != 1 {
			t.Fatalf("vm1 has %d lookaside records, want 1", len(recs))
		}
		if d := recordProp(recs[0], "depth"); d != "0" {
			t.Errorf("vm1 depth = %q, want 0", d)
		}
		if id := recordProp(recs[0], "ddb.longContentID"); id != contentIDV1 {
			t.Errorf("vm1 ddb.longContentID = %q, want %q", id, contentIDV1)
		}
		if cid := recordProp(recs[0], "cid"); cid != contentIDV1[24:] {
			t.Errorf("vm1 cid = %q, want %q", cid, contentIDV1[24:])
		}
		if v := extraConfigValue(ctx, t, c, vm1.Reference(), containerKey); v != "image:v1" {
			t.Errorf("vm1 %s = %q, want image:v1", containerKey, v)
		}
		if v := extraConfigValue(ctx, t, c, vm1.Reference(), scopeKey); v != scopeValue {
			t.Errorf("vm1 %s = %q, want the client's %q", scopeKey, v, scopeValue)
		}
		if r := lookaside("vm1", contentV1Rule); !r.Matched {
			t.Errorf("vm1 %s lookaside Matched = false, want true", contentV1Rule)
		}
		if r := lookaside("vm1", contentV2Rule); r.Matched {
			t.Errorf("vm1 %s lookaside Matched = true, want false", contentV2Rule)
		}

		// A content ID no specific rule names takes the wildcard rule.
		stage(itemPath(i2, "item-2-disk1.vmdk"), "vm2")
		vm2 := create("vm2", "vm2", scopeValue)
		if v := extraConfigValue(ctx, t, c, vm2.Reference(), containerKey); v != "image:default" {
			t.Errorf("vm2 %s = %q, want image:default", containerKey, v)
		}

		// An empty disk has no content ID, so no content rule patches the VM and power-on faults.
		mkdir("vm3")
		vdm := object.NewVirtualDiskManager(c)
		tk, err := vdm.CreateVirtualDisk(ctx, "[LocalDS_0] vm3/disk-0.vmdk", dc, &types.FileBackedVirtualDiskSpec{
			VirtualDiskSpec: types.VirtualDiskSpec{AdapterType: "lsiLogic", DiskType: "thin"},
			CapacityKb:      1024,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err = tk.Wait(ctx); err != nil {
			t.Fatal(err)
		}
		vm3 := create("vm3", "vm3", scopeValue)
		if v := extraConfigValue(ctx, t, c, vm3.Reference(), containerKey); v != "" {
			t.Errorf("vm3 %s = %q, want none", containerKey, v)
		}
		pwr, err := vm3.PowerOn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err = pwr.Wait(ctx); err == nil {
			t.Fatal("vm3 PowerOn succeeded, want a fault")
		} else {
			var te task.Error
			if !errors.As(err, &te) {
				t.Fatalf("vm3 PowerOn error = %T, want task.Error", err)
			}
			f, ok := te.Fault().(*types.GenericVmConfigFault)
			if !ok {
				t.Fatalf("vm3 PowerOn fault = %T, want *types.GenericVmConfigFault", te.Fault())
			}
			for _, want := range []string{requireRule, "powerOn", "vm3"} {
				if !strings.Contains(f.Reason, want) {
					t.Errorf("vm3 fault reason %q does not contain %q", f.Reason, want)
				}
			}
		}
		if r := lookaside("vm3", contentAnyRule); !slices.Contains(r.Value.(types.ArrayOfString).String, "ddb.longContentID=<absent>") {
			t.Errorf("vm3 %s lookaside = %v, want ddb.longContentID=<absent>", contentAnyRule, r.Value)
		}
		if state, err := vm3.PowerState(ctx); err != nil || state != types.VirtualMachinePowerStatePoweredOff {
			t.Errorf("vm3 power state = %v, %v; want poweredOff", state, err)
		}

		// A second session replaces the item's disk with content ID v2. A new VM takes v2, and vm1,
		// whose disk was copied before, keeps v1.
		uploadItemFiles(ctx, t, m, i1, itemFile{itemDiskName, contentIDDisk(t, contentIDV2)})
		mkdir("cache-v2")
		if err := copyThinDisk(ctx, c, dc, itemPath(i1, itemDiskName), "[LocalDS_0] cache-v2/disk.vmdk"); err != nil {
			t.Fatal(err)
		}
		stage("[LocalDS_0] cache-v2/disk.vmdk", "vm4")
		vm4 := create("vm4", "vm4", scopeValue)
		if v := extraConfigValue(ctx, t, c, vm4.Reference(), containerKey); v != "image:v2" {
			t.Errorf("vm4 %s = %q, want image:v2", containerKey, v)
		}
		recs = records(vm1)
		if len(recs) != 1 || recordProp(recs[0], "ddb.longContentID") != contentIDV1 {
			t.Errorf("vm1 records changed after the second session: %v", recs)
		}
		if r := lookaside("vm1", contentV1Rule); !r.Matched {
			t.Errorf("vm1 %s lookaside Matched = false after the second session, want true", contentV1Rule)
		}

		// A VM outside the scope takes only the rule that names it.
		stage("[LocalDS_0] cache-v1/disk.vmdk", "vm5")
		vm5 := create("unscoped-vm", "vm5", "")
		if v := extraConfigValue(ctx, t, c, vm5.Reference(), containerKey); v != "image:by-name" {
			t.Errorf("unscoped-vm %s = %q, want image:by-name", containerKey, v)
		}

		// DeployLibraryItem copies the disk without a content ID.
		ref, err := vcenter.NewManager(rc).DeployLibraryItem(ctx, i1, vcenter.Deploy{
			DeploymentSpec: vcenter.DeploymentSpec{
				Name:            "deployed-vm",
				NetworkMappings: []vcenter.NetworkMapping{{Key: "nat", Value: dvpg.Reference().Value}},
			},
			Target: vcenter.Target{ResourcePoolID: pool.Reference().Value, FolderID: folder.Reference().Value},
		})
		if err != nil {
			t.Fatal(err)
		}
		recs = records(object.NewVirtualMachine(c, *ref))
		if len(recs) == 0 {
			t.Error("deployed-vm has no lookaside records")
		}
		for _, r := range recs {
			if id := recordProp(r, "ddb.longContentID"); id != "" {
				t.Errorf("deployed-vm ddb.longContentID = %q, want none", id)
			}
		}
	}, model)
}
