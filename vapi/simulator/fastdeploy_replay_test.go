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
	"sync"
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
	replayIDV1 = "0123456789abcdef0123456789abcd01"
	replayIDV2 = "0123456789abcdef0123456789abcd02"
	replayIDI2 = "0123456789abcdef0123456789abcd03"

	replayNode1     = "example-node-" + replayIDV1
	replayNode2     = "example-node-" + replayIDV2
	replayLookaside = "lookaside:" + simulator.LookasideDiskDescriptor

	replayPhoton = "photon-5-amd64-v1.35.5---vmware.1-skr.1"
	replayNS     = "example-system"
)

// replayStub is a 1 MiB stream-optimized disk whose descriptor carries id as ddb.longContentID and
// whose CID is the last 8 hex digits of id, as the harness stamps it.
func replayStub(t *testing.T, id string) []byte {
	t.Helper()
	cid, err := strconv.ParseUint(id[len(id)-8:], 16, 32)
	if err != nil {
		t.Fatal(err)
	}
	name := filepath.Join(t.TempDir(), "stub.vmdk")
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

type replayFile struct {
	name string
	data []byte
}

// replayUpload adds files to one update session of the library item and completes it.
func replayUpload(ctx context.Context, t *testing.T, m *library.Manager, itemID string, files ...replayFile) {
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

// replayPath is the datastore path of a library item file.
func replayPath(libID, itemID, file string) string {
	return fmt.Sprintf("[LocalDS_0] contentlib-%s/%s/%s", libID, itemID, file)
}

// replaySpec decodes the captured CreateVM spec and rebases it onto name, dir and namespace. An empty
// ns removes vmservice.namespacedName, as a VM that is not a service VM has none.
func replaySpec(t *testing.T, name, dir, ns string) *types.VirtualMachineConfigSpec {
	t.Helper()
	f, err := os.Open("testdata/replay-vmop/createvm-configspec.json")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var spec types.VirtualMachineConfigSpec
	if err = types.NewJSONDecoder(f).Decode(&spec); err != nil {
		t.Fatal(err)
	}
	spec.Name = name
	spec.Files.VmPathName = fmt.Sprintf("[LocalDS_0] %s/%s.vmx", dir, name)
	for _, c := range spec.DeviceChange {
		dc := c.GetVirtualDeviceConfigSpec()
		if disk, ok := dc.Device.(*types.VirtualDisk); ok {
			disk.Backing.(*types.VirtualDiskFlatVer2BackingInfo).FileName = fmt.Sprintf("[LocalDS_0] %s/disk-0.vmdk", dir)
		}
	}
	var ec []types.BaseOptionValue
	for _, o := range spec.ExtraConfig {
		if o.GetOptionValue().Key == "vmservice.namespacedName" {
			if ns == "" {
				continue
			}
			o.GetOptionValue().Value = ns + "/" + name
		}
		ec = append(ec, o)
	}
	spec.ExtraConfig = ec
	return &spec
}

// replayEc reads one ExtraConfig value of vm, or "" when absent.
func replayEc(ctx context.Context, t *testing.T, c *vim25.Client, vm types.ManagedObjectReference, key string) string {
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

// replayCopy copies src to dst as a thin lsiLogic disk, as vm-operator's cache and clone copies do.
func replayCopy(ctx context.Context, c *vim25.Client, dc *object.Datacenter, src, dst string) error {
	vdm := object.NewVirtualDiskManager(c)
	destSpec := &types.FileBackedVirtualDiskSpec{VirtualDiskSpec: types.VirtualDiskSpec{AdapterType: "lsiLogic", DiskType: "thin"}}
	tk, err := vdm.CopyVirtualDisk(ctx, src, dc, dst, dc, destSpec, false)
	if err != nil {
		return err
	}
	return tk.Wait(ctx)
}

// replayProp reads one property of a lookaside record, or "" when the record lacks it.
func replayProp(rec simulator.LookasideRecord, name string) string {
	for _, p := range rec {
		if p.Name == name {
			return fmt.Sprint(p.Val)
		}
	}
	return ""
}

// TestFastDeployReplay replays the vm-operator CreateVM and PowerOn sequence captured from a run against
// VMRules: a library item, concurrent cache copies, linked and empty disks, a second item session, and
// the bootstrap VM. It is the gate before any end-to-end run.
func TestFastDeployReplay(t *testing.T) {
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

		// Step 1: library and items.
		m := library.NewManager(rc)
		libID, err := m.CreateLibrary(ctx, library.Library{
			Name:    "replay-lib",
			Type:    "LOCAL",
			Storage: []library.StorageBacking{{DatastoreID: ds.Reference().Value, Type: "DATASTORE"}},
		})
		if err != nil {
			t.Fatal(err)
		}
		i1, err := m.CreateLibraryItem(ctx, library.Item{Name: "sb-103603714-photon-5-amd64-v1.35.5---vmware.1-skr.1", Type: library.ItemTypeOVF, LibraryID: libID})
		if err != nil {
			t.Fatal(err)
		}
		ovf, err := os.ReadFile("../../ovf/fixtures/ttylinux.ovf")
		if err != nil {
			t.Fatal(err)
		}
		desc := strings.ReplaceAll(string(ovf), `ovf:name="nat"`, `ovf:name="Management Network"`)
		desc = strings.ReplaceAll(desc, "<rasd:Connection>nat</rasd:Connection>", "<rasd:Connection>Management Network</rasd:Connection>")
		desc = strings.ReplaceAll(desc, "ttylinux-pc_i486-16.1-disk1.vmdk", replayPhoton+"-disk1.vmdk")
		replayUpload(ctx, t, m, i1,
			replayFile{replayPhoton + ".ovf", []byte(desc)},
			replayFile{replayPhoton + "-disk1.vmdk", replayStub(t, replayIDV1)})
		i2, err := m.CreateLibraryItem(ctx, library.Item{Name: "item-2", Type: library.ItemTypeOVF, LibraryID: libID})
		if err != nil {
			t.Fatal(err)
		}
		replayUpload(ctx, t, m, i2, replayFile{"item-2-disk1.vmdk", replayStub(t, replayIDI2)})
		lp1 := replayPath(libID, i1, replayPhoton+"-disk1.vmdk")
		lp2 := replayPath(libID, i2, "item-2-disk1.vmdk")

		// Step 2: folders.
		dcFolder, err := finder.DefaultFolder(ctx)
		if err != nil {
			t.Fatal(err)
		}
		exFolder, err := dcFolder.CreateFolder(ctx, "example-sim-test")
		if err != nil {
			t.Fatal(err)
		}
		sup, err := exFolder.CreateFolder(ctx, replayNS)
		if err != nil {
			t.Fatal(err)
		}

		// Step 3: rules, in order, before any CreateVM.
		S := func() property.Match {
			return property.Match{`config.extraConfig["vmservice.namespacedName"]`: replayNS + "/*"}
		}
		X := func() property.Match { return property.Match{`config.extraConfig["RUN.container"]`: "*"} }
		dd := func(id string) []simulator.LookasideMatch {
			return []simulator.LookasideMatch{{View: simulator.LookasideDiskDescriptor, Match: property.Match{"ddb.longContentID": id}}}
		}
		patch := func(image string) *types.VirtualMachineConfigSpec {
			return &types.VirtualMachineConfigSpec{ExtraConfig: []types.BaseOptionValue{
				&types.OptionValue{Key: "RUN.container", Value: image}}}
		}
		rules := []simulator.VMRule{
			{Name: "example-bootstrap", Phases: []simulator.VMRulePhase{simulator.VMRulePhaseCreate},
				Match: simulator.ObjectMatch{Include: property.Match{"name": "example-sim-test-bootstrap-vm"}, Exclude: X()},
				Patch: patch("example-sim-bootstrap:test")},
			{Name: replayNode1, Match: simulator.ObjectMatch{Include: S(), Lookaside: dd(replayIDV1), Exclude: X()},
				Patch: patch("node:v1")},
			{Name: replayNode2, Match: simulator.ObjectMatch{Include: S(), Lookaside: dd(replayIDV2), Exclude: X()},
				Patch: patch("node:v2")},
			{Name: "example-node-default", Match: simulator.ObjectMatch{Include: S(), Lookaside: dd("*"), Exclude: X()},
				Patch: patch("node:default")},
			{Name: "example-require-container", Phases: []simulator.VMRulePhase{simulator.VMRulePhasePowerOn},
				Match: simulator.ObjectMatch{Include: S(), Exclude: X()},
				Fault: "VM in guard scope has no container config: no example-node rule matched a disk content ID (disks created empty or by DeployLibraryItem have none)"},
		}
		for _, r := range rules {
			if err := model.Map().VMRules.Add(r); err != nil {
				t.Fatal(err)
			}
		}

		// stage copies src into a new directory dir as its disk-0.vmdk.
		stage := func(src, dir string) {
			t.Helper()
			fm := object.NewFileManager(c)
			if err := fm.MakeDirectory(ctx, "[LocalDS_0] "+dir, dc, true); err != nil {
				t.Fatal(err)
			}
			if err := replayCopy(ctx, c, dc, src, "[LocalDS_0] "+dir+"/disk-0.vmdk"); err != nil {
				t.Fatal(err)
			}
		}
		create := func(f *object.Folder, name, dir, ns string) *object.VirtualMachine {
			t.Helper()
			task, err := f.CreateVM(ctx, *replaySpec(t, name, dir, ns), pool, nil)
			if err != nil {
				t.Fatal(err)
			}
			info, err := task.WaitForResult(ctx)
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
		explain := func(name string, phase simulator.VMRulePhase) []simulator.RuleExplanation {
			t.Helper()
			exps, err := model.Map().VMRules.ExplainVM(model.Map(), name, phase)
			if err != nil {
				t.Fatal(err)
			}
			return exps
		}
		// lookaside returns the lookaside result of rule in exps, which must be present.
		lookaside := func(exps []simulator.RuleExplanation, rule string) simulator.MatchResult {
			t.Helper()
			for _, e := range exps {
				if e.Rule != rule {
					continue
				}
				for _, r := range e.Results {
					if r.Key == replayLookaside {
						return r
					}
				}
			}
			t.Fatalf("no %s result for rule %s", replayLookaside, rule)
			return simulator.MatchResult{}
		}

		// Step 4: two concurrent copies of the same cache file; at least one succeeds.
		vdmDirs := "[LocalDS_0] vmi-cache-v1"
		if err := object.NewFileManager(c).MakeDirectory(ctx, vdmDirs, dc, true); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		errs := make([]error, 2)
		for i := range errs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				errs[i] = replayCopy(ctx, c, dc, lp1, vdmDirs+"/disk.vmdk")
			}()
		}
		wg.Wait()
		if errs[0] != nil && errs[1] != nil {
			t.Fatalf("both cache copies failed: %v; %v", errs[0], errs[1])
		}

		// Step 5: VM1 carries node v1; its ID survives upload and the DestSpec copies.
		stage(vdmDirs+"/disk.vmdk", "1b37d647-0dc0-4edc-bb33-2ce46c1ec5b8")
		vm1 := create(sup, "example-wc4xf-fcxkf", "1b37d647-0dc0-4edc-bb33-2ce46c1ec5b8", replayNS)
		recs := records(vm1)
		if len(recs) != 1 {
			t.Fatalf("VM1 has %d lookaside records, want 1", len(recs))
		}
		if d := replayProp(recs[0], "depth"); d != "0" {
			t.Errorf("VM1 depth = %q, want 0", d)
		}
		if id := replayProp(recs[0], "ddb.longContentID"); id != replayIDV1 {
			t.Errorf("VM1 ddb.longContentID = %q, want %q", id, replayIDV1)
		}
		if cid := replayProp(recs[0], "cid"); cid != replayIDV1[24:] {
			t.Errorf("VM1 cid = %q, want %q", cid, replayIDV1[24:])
		}
		if v := replayEc(ctx, t, c, vm1.Reference(), "RUN.container"); v != "node:v1" {
			t.Errorf("VM1 RUN.container = %q, want node:v1", v)
		}
		for _, o := range replaySpec(t, "example-wc4xf-fcxkf", "1b37d647-0dc0-4edc-bb33-2ce46c1ec5b8", replayNS).ExtraConfig {
			ov := o.GetOptionValue()
			if got := replayEc(ctx, t, c, vm1.Reference(), ov.Key); got != fmt.Sprint(ov.Value) {
				t.Errorf("VM1 %s = %q, want fixture value %q", ov.Key, got, ov.Value)
			}
		}
		exps := explain("example-wc4xf-fcxkf", simulator.VMRulePhasePowerOn)
		if r := lookaside(exps, replayNode1); !r.Matched {
			t.Errorf("%s lookaside Matched = false, want true", replayNode1)
		}
		if r := lookaside(exps, replayNode2); r.Matched {
			t.Errorf("%s lookaside Matched = true, want false", replayNode2)
		}

		// Step 6: VM2 from the item-2 disk takes the default node rule.
		stage(lp2, "vm2")
		vm2 := create(sup, "example-vm2", "vm2", replayNS)
		if v := replayEc(ctx, t, c, vm2.Reference(), "RUN.container"); v != "node:default" {
			t.Errorf("VM2 RUN.container = %q, want node:default", v)
		}

		// Step 7: VM3 from an empty disk.
		if err := object.NewFileManager(c).MakeDirectory(ctx, "[LocalDS_0] vm3", dc, true); err != nil {
			t.Fatal(err)
		}
		vdm := object.NewVirtualDiskManager(c)
		vm3Disk, err := vdm.CreateVirtualDisk(ctx, "[LocalDS_0] vm3/disk-0.vmdk", dc, &types.FileBackedVirtualDiskSpec{
			VirtualDiskSpec: types.VirtualDiskSpec{AdapterType: "lsiLogic", DiskType: "thin"},
			CapacityKb:      1024,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err = vm3Disk.Wait(ctx); err != nil {
			t.Fatal(err)
		}
		vm3 := create(sup, "example-vm3", "vm3", replayNS)
		if v := replayEc(ctx, t, c, vm3.Reference(), "RUN.container"); v != "" {
			t.Errorf("VM3 RUN.container = %q, want none", v)
		}
		pwr, err := vm3.PowerOn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err = pwr.Wait(ctx); err == nil {
			t.Fatal("VM3 PowerOn succeeded, want a fault")
		} else {
			var te task.Error
			if !errors.As(err, &te) {
				t.Fatalf("VM3 PowerOn error = %T, want task.Error", err)
			}
			f, ok := te.Fault().(*types.GenericVmConfigFault)
			if !ok {
				t.Fatalf("VM3 PowerOn fault = %T, want *types.GenericVmConfigFault", te.Fault())
			}
			for _, want := range []string{"example-require-container", "powerOn", "example-vm3"} {
				if !strings.Contains(f.Reason, want) {
					t.Errorf("VM3 fault reason %q does not contain %q", f.Reason, want)
				}
			}
		}
		exps = explain("example-vm3", simulator.VMRulePhasePowerOn)
		for _, e := range exps {
			if e.Rule == "example-require-container" && !e.Applies {
				t.Errorf("example-require-container does not apply to example-vm3")
			}
		}
		if r := lookaside(exps, "example-node-default"); !slices.Contains(r.Value.(types.ArrayOfString).String, "ddb.longContentID=<absent>") {
			t.Errorf("VM3 example-node-default lookaside = %v, want ddb.longContentID=<absent>", r.Value)
		}
		if state, err := vm3.PowerState(ctx); err != nil || state != types.VirtualMachinePowerStatePoweredOff {
			t.Errorf("VM3 power state = %v, %v; want poweredOff", state, err)
		}

		// Step 8: a second session on I1 replaces disk1; VM4 takes node v2.
		replayUpload(ctx, t, m, i1, replayFile{replayPhoton + "-disk1.vmdk", replayStub(t, replayIDV2)})
		vmi2 := "[LocalDS_0] vmi-cache-v2"
		if err := object.NewFileManager(c).MakeDirectory(ctx, vmi2, dc, true); err != nil {
			t.Fatal(err)
		}
		if err := replayCopy(ctx, c, dc, lp1, vmi2+"/disk.vmdk"); err != nil {
			t.Fatal(err)
		}
		stage(vmi2+"/disk.vmdk", "vm4")
		vm4 := create(sup, "example-vm4", "vm4", replayNS)
		if v := replayEc(ctx, t, c, vm4.Reference(), "RUN.container"); v != "node:v2" {
			t.Errorf("VM4 RUN.container = %q, want node:v2", v)
		}
		recs = records(vm1)
		if len(recs) != 1 || replayProp(recs[0], "ddb.longContentID") != replayIDV1 {
			t.Errorf("VM1 records changed after session 2: %v", recs)
		}
		if v := replayEc(ctx, t, c, vm1.Reference(), "RUN.container"); v != "node:v1" {
			t.Errorf("VM1 RUN.container = %q after session 2, want node:v1", v)
		}
		exps = explain("example-wc4xf-fcxkf", simulator.VMRulePhasePowerOn)
		if r := lookaside(exps, replayNode1); !r.Matched {
			t.Errorf("VM1 %s lookaside Matched = false after session 2, want true", replayNode1)
		}
		if r := lookaside(exps, replayNode2); r.Matched {
			t.Errorf("VM1 %s lookaside Matched = true after session 2, want false", replayNode2)
		}

		// Step 9: the bootstrap VM has no namespace and takes the bootstrap image.
		stage(vdmDirs+"/disk.vmdk", "vm5")
		vm5 := create(exFolder, "example-sim-test-bootstrap-vm", "vm5", "")
		if v := replayEc(ctx, t, c, vm5.Reference(), "RUN.container"); v != "example-sim-bootstrap:test" {
			t.Errorf("VM5 RUN.container = %q, want example-sim-bootstrap:test", v)
		}

		// Step 10: deploy I1 with DeployLibraryItem; the copied disk carries no content ID.
		ref, err := vcenter.NewManager(rc).DeployLibraryItem(ctx, i1, vcenter.Deploy{
			DeploymentSpec: vcenter.DeploymentSpec{
				Name: "example-vm6",
				NetworkMappings: []vcenter.NetworkMapping{{
					Key:   "Management Network",
					Value: dvpg.Reference().Value,
				}},
			},
			Target: vcenter.Target{ResourcePoolID: pool.Reference().Value, FolderID: sup.Reference().Value},
		})
		if err != nil {
			t.Fatal(err)
		}
		vm6 := object.NewVirtualMachine(c, *ref)
		recs = records(vm6)
		if len(recs) == 0 {
			t.Error("VM6 has no lookaside records")
		}
		for _, r := range recs {
			if id := replayProp(r, "ddb.longContentID"); id != "" {
				t.Errorf("VM6 ddb.longContentID = %q, want none", id)
			}
		}
	}, model)
}
