// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package simulator

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/test"
	"github.com/vmware/govmomi/vim25/methods"
	"github.com/vmware/govmomi/vim25/types"
)

func TestSystemdMountUnit(t *testing.T) {
	for _, p := range []string{"/var/lib/etcd", "/mnt/a-b", "/data//x/", "/with space", "/.hidden/x", "/a_b:c.d"} {
		got := systemdMountUnit(p)
		if bin, err := exec.LookPath("systemd-escape"); err == nil {
			want, err := exec.Command(bin, "--path", "--suffix=mount", p).Output()
			require.NoError(t, err)
			require.Equal(t, strings.TrimSpace(string(want)), got, p)
		}
	}
	require.Equal(t, "var-lib-etcd.mount", systemdMountUnit("/var/lib/etcd"))
	require.Equal(t, "-.mount", systemdMountUnit("/"))
}

// userdataFor returns cloud-config user-data whose machine-agent conf holds
// one volumeMount, in the named encoding.
func userdataFor(t *testing.T, encoding, mountPoint string, bus, target int) []types.BaseOptionValue {
	conf := fmt.Sprintf("apiVersion: cluster.kubernetes.vmware.com/v1alpha1\nkind: MachineConfig\nspec:\n  volumeMounts:\n  - mountPoint: %s\n    pcidevice:\n      bus: %d\n      target: %d\n", mountPoint, bus, target)
	cc, err := yaml.Marshal(map[string]any{"write_files": []map[string]any{
		{"path": "/etc/other", "content": "volumeMounts:\n- mountPoint: /elsewhere\n  pcidevice: {bus: 0, target: 9}\n"},
		{"path": machineAgentConf, "content": conf},
	}})
	require.NoError(t, err)
	data := string(cc)
	switch encoding {
	case "base64":
		data = base64.StdEncoding.EncodeToString(cc)
	case "gzip+base64":
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		_, err := zw.Write(cc)
		require.NoError(t, err)
		require.NoError(t, zw.Close())
		data = base64.StdEncoding.EncodeToString(buf.Bytes())
	}
	return []types.BaseOptionValue{
		&types.OptionValue{Key: userdataKey, Value: data},
		&types.OptionValue{Key: userdataEncodingKey, Value: encoding},
	}
}

func mustGuestinfo(t *testing.T, opts []types.BaseOptionValue) []byte {
	t.Helper()
	text, err := guestinfoUserdata(opts)
	require.NoError(t, err)
	return text
}

// The placeholder a rolled Supervisor VM carries in guestinfo is no user-data.
func TestGuestinfoUserdataPlaceholder(t *testing.T) {
	text, err := guestinfoUserdata([]types.BaseOptionValue{
		&types.OptionValue{Key: userdataKey, Value: "---"},
		&types.OptionValue{Key: userdataEncodingKey, Value: "gzip+base64"},
	})
	require.NoError(t, err)
	require.Empty(t, text)
}

func TestDiskMountPoints(t *testing.T) {
	for _, enc := range []string{"", "base64", "gzip+base64"} {
		got, err := diskMountPoints(mustGuestinfo(t, userdataFor(t, enc, "/var/lib/etcd/", 3, 1)))
		require.NoError(t, err, enc)
		require.Equal(t, map[scsiSlot]string{{3, 1}: "/var/lib/etcd"}, got, enc)
	}

	none, err := diskMountPoints(nil)
	require.NoError(t, err)
	require.Empty(t, none)
	none, err = diskMountPoints([]byte("runcmd: [true]\n"))
	require.NoError(t, err)
	require.Empty(t, none, "user-data without machine-agent conf")

	for name, opts := range map[string][]types.BaseOptionValue{
		"relative": userdataFor(t, "", "rel", 3, 1),
		"root":     userdataFor(t, "", "/", 3, 1),
		"bad encoding": {&types.OptionValue{Key: userdataKey, Value: "x"},
			&types.OptionValue{Key: userdataEncodingKey, Value: "rot13"}},
		"bad gzip": {&types.OptionValue{Key: userdataKey, Value: "eA=="},
			&types.OptionValue{Key: userdataEncodingKey, Value: "gzip+base64"}},
	} {
		text, err := guestinfoUserdata(opts)
		if err == nil {
			_, err = diskMountPoints(text)
		}
		require.Error(t, err, name)
	}
}

// TestVMDiskMounts: an attached FCD at the named bus and unit is presented as
// its content directory, other units are not, and detach removes it.
func TestVMDiskMounts(t *testing.T) {
	testFCD(t, func(e *fcdEnv) {
		vm := e.vm(0)
		ctx := registryContext(e.model.Map())
		vm.Config.ExtraConfig = append(vm.Config.ExtraConfig,
			userdataFor(t, "gzip+base64", "/var/lib/etcd", 0, 1)...)

		none, err := vm.diskMounts(ctx)
		require.NoError(t, err)
		require.Empty(t, none)

		id := e.createFCD("etcd")
		other := e.createFCD("other")
		unit := int32(1)
		require.NoError(t, e.attach(vm, id, 0, &unit))
		unit = 2
		require.NoError(t, e.attach(vm, other, 0, &unit))

		got, err := vm.diskMounts(ctx)
		require.NoError(t, err)
		want := strings.TrimSuffix(e.ds.resolve(ctx, strings.SplitN(e.fcdPath(id), "] ", 2)[1]), ".vmdk") + ".d"
		require.Equal(t, []diskMount{{source: want, dest: "/var/lib/etcd"}}, got)
		require.DirExists(t, filepath.Clean(want))

		require.NoError(t, e.detach(vm, id))
		got, err = vm.diskMounts(ctx)
		require.NoError(t, err)
		require.Empty(t, got)
		_, err = os.Stat(want)
		require.NoError(t, err, "detach keeps the FCD's content")
	})
}

// TestContainerVM_DiskMount checks that a disk attached at the named bus and
// unit before power-on reaches the guest as its content directory with the
// guest's .mount unit, and that detach while stopped undoes both at the next
// power-on.
func TestContainerVM_DiskMount(t *testing.T) {
	test.RequireContainerOptIn(t)
	if !test.HasDocker() {
		t.Skip("requires docker or podman (aliased as docker) on linux")
	}

	env, vm, svm, id, attach := diskContainerSetup(t, "disk-mount")
	attach()

	powerOn(t, env, vm)
	cid := containerID(t, env, svm)
	unitFile := "/etc/systemd/system/var-lib-etcd.mount"
	out, err := dockerExec(env.goCtx, cid, "cat", unitFile)
	require.NoError(t, err)
	require.Contains(t, out, "Where=/var/lib/etcd")

	_, err = dockerExec(env.goCtx, cid, "sh", "-c", "echo x > /var/lib/etcd/probe")
	require.NoError(t, err)
	var dir string
	env.simCtx.WithLock(svm, func() { dir = svm.svm.diskLive[0].source })
	require.FileExists(t, filepath.Join(dir, "probe"))

	powerOff(t, env, vm)
	require.NoError(t, vm.DetachDisk(env.goCtx, id.Id))
	powerOn(t, env, vm)
	cid = containerID(t, env, svm)
	_, err = dockerExec(env.goCtx, cid, "test", "!", "-e", unitFile)
	require.NoError(t, err, "detach removes the unit at the next power-on")
	_, err = dockerExec(env.goCtx, cid, "test", "!", "-e", "/var/lib/etcd/probe")
	require.NoError(t, err)
	powerOff(t, env, vm)
}

// diskContainerSetup creates a container VM whose guest asks for /var/lib/etcd at bus 0, unit 1,
// with a controller and an FCD ready; attach attaches the FCD at that slot.
func diskContainerSetup(t *testing.T, name string) (env *vcSimEnv, vm *object.VirtualMachine, svm *VirtualMachine, id types.ID, attach func()) {
	t.Helper()
	test.RequireContainerOptIn(t)
	if !test.HasDocker() {
		t.Skip("requires docker or podman (aliased as docker) on linux")
	}

	env = newVCSIMEnv(t)
	extra := append([]types.BaseOptionValue{
		&types.OptionValue{Key: "RUN.nestedContainers", Value: "true"},
		&types.OptionValue{Key: "RUN.nestedVolumes", Value: `["/var/lib/etcd"]`},
	}, userdataFor(t, "gzip+base64", "/var/lib/etcd", 0, 1)...)
	vm, svm = createContainerVM(t, env, name, "", extra...)

	ctrl, err := object.VirtualDeviceList(nil).CreateSCSIController("pvscsi")
	require.NoError(t, err)
	task, err := vm.Reconfigure(env.goCtx, types.VirtualMachineConfigSpec{DeviceChange: []types.BaseVirtualDeviceConfigSpec{
		&types.VirtualDeviceConfigSpec{Operation: types.VirtualDeviceConfigSpecOperationAdd, Device: ctrl},
	}})
	require.NoError(t, err)
	require.NoError(t, task.Wait(env.goCtx))

	ds := env.simCtx.Map.Any("Datastore").(*Datastore)
	id = createFCDOn(t, env, ds, "etcd")
	attach = func() {
		unit := int32(1)
		require.NoError(t, vm.AttachDisk(env.goCtx, id.Id, object.NewDatastore(env.client.Client, ds.Self), 0, &unit))
	}
	return env, vm, svm, id, attach
}

// createFCDOn creates a first class disk on ds.
func createFCDOn(t *testing.T, env *vcSimEnv, ds *Datastore, name string) types.ID {
	t.Helper()
	res, err := methods.CreateDisk_Task(env.goCtx, env.client.Client, &types.CreateDisk_Task{
		This: *env.client.ServiceContent.VStorageObjectManager,
		Spec: types.VslmCreateSpec{Name: name, CapacityInMB: 10, BackingSpec: &types.VslmCreateSpecDiskFileBackingSpec{
			VslmCreateSpecBackingSpec: types.VslmCreateSpecBackingSpec{Datastore: ds.Self},
		}},
	})
	require.NoError(t, err)
	info, err := object.NewTask(env.client.Client, res.Returnval).WaitForResult(env.goCtx)
	require.NoError(t, err)
	return info.Result.(types.VStorageObject).Config.Id
}

// TestContainerVM_DiskHotAttach: a disk attached or detached while the container runs reaches the
// guest at once, with its unit; after a stop it is presented again at the next start; a disk on a
// datastore made after the container is presented; and the guest sees nothing else of the datastore.
func TestContainerVM_DiskHotAttach(t *testing.T) {
	env, vm, svm, id, attach := diskContainerSetup(t, "disk-hot")
	unitFile := "/etc/systemd/system/var-lib-etcd.mount"

	powerOn(t, env, vm)
	cid := containerID(t, env, svm)
	_, err := dockerExec(env.goCtx, cid, "test", "!", "-e", unitFile)
	require.NoError(t, err, "no disk yet")

	present := func(cid string) {
		t.Helper()
		out, err := dockerExec(env.goCtx, cid, "cat", unitFile)
		require.NoError(t, err)
		require.Contains(t, out, "Where=/var/lib/etcd")
		_, err = dockerExec(env.goCtx, cid, "sh", "-c", "echo x > /var/lib/etcd/probe")
		require.NoError(t, err)
	}
	absent := func(cid string) {
		t.Helper()
		_, err := dockerExec(env.goCtx, cid, "test", "!", "-e", unitFile)
		require.NoError(t, err)
		_, err = dockerExec(env.goCtx, cid, "test", "!", "-e", "/var/lib/etcd/probe")
		require.NoError(t, err)
	}

	attach()
	present(cid)
	var dir string
	env.simCtx.WithLock(svm, func() { dir = svm.svm.diskLive[0].source })
	require.FileExists(t, filepath.Join(dir, "probe"))

	require.NoError(t, vm.DetachDisk(env.goCtx, id.Id))
	absent(cid)

	attach()
	powerOff(t, env, vm)
	powerOn(t, env, vm)
	present(containerID(t, env, svm))

	cid = containerID(t, env, svm)
	ds := env.simCtx.Map.Any("Datastore").(*Datastore)
	require.NoError(t, os.WriteFile(filepath.Join(ds.Path(), "other-vm-secret"), nil, 0o644))
	out, err := dockerExec(env.goCtx, cid, "sh", "-c", "find / -name other-vm-secret 2>/dev/null; ls -d /.vcsim 2>/dev/null; ls /var/lib/etcd")
	require.NoError(t, err)
	require.Equal(t, "probe", out, "the guest sees the disk's directory and no other datastore content")

	require.NoError(t, vm.DetachDisk(env.goCtx, id.Id))
	absent(containerID(t, env, svm))

	// A datastore made after the container was created.
	late := t.TempDir()
	hs := env.simCtx.Map.Get(*svm.Runtime.Host).(*HostSystem)
	_, err = methods.CreateLocalDatastore(env.goCtx, env.client.Client, &types.CreateLocalDatastore{
		This: *hs.ConfigManager.DatastoreSystem, Name: "late-" + t.Name(), Path: late,
	})
	require.NoError(t, err)
	lateDS := env.simCtx.Map.FindByName("late-"+t.Name(), hs.Datastore).(*Datastore)
	lateID := createFCDOn(t, env, lateDS, "late")
	unit := int32(1)
	require.NoError(t, vm.AttachDisk(env.goCtx, lateID.Id, object.NewDatastore(env.client.Client, lateDS.Self), 0, &unit))
	present(cid)
	require.NoError(t, vm.DetachDisk(env.goCtx, lateID.Id))
	absent(cid)
	powerOff(t, env, vm)
}

func TestDiskPlan(t *testing.T) {
	a := diskMount{source: "/d/a.d", dest: "/a"}
	b := diskMount{source: "/d/b.d", dest: "/b"}
	a2 := diskMount{source: "/d/a2.d", dest: "/a"}

	add, remove := diskPlan(nil, []diskMount{a})
	require.Equal(t, []diskMount{a}, add)
	require.Empty(t, remove)

	add, remove = diskPlan([]diskMount{a, b}, []diskMount{b})
	require.Empty(t, add)
	require.Equal(t, []diskMount{a}, remove)

	add, remove = diskPlan([]diskMount{a}, []diskMount{a2})
	require.Equal(t, []diskMount{a2}, add)
	require.Equal(t, []diskMount{a}, remove, "a changed source is a remove and an add")

	add, remove = diskPlan([]diskMount{a, b}, []diskMount{a, b})
	require.Empty(t, add)
	require.Empty(t, remove)
}

func TestDiskTreeHelperUsage(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux only")
	}
	require.Error(t, diskTreeHelper([]string{"1"}))
}

func TestStaleUnits(t *testing.T) {
	a, b := diskMount{source: "/d/a.d", dest: "/a"}, diskMount{source: "/d/b.d", dest: "/b"}
	require.Equal(t, []string{b.unitPath()}, staleUnits([]string{a.unitPath(), b.unitPath()}, []diskMount{a}))
	require.Empty(t, staleUnits([]string{a.unitPath()}, []diskMount{{source: "/d/a2.d", dest: "/a"}}), "same mount point, same unit")
	require.Empty(t, staleUnits(nil, []diskMount{a}))
}

// TestVMDiskMountsCloudinitPrep drives the production path: guestinfo.userdata is the placeholder
// "---", the user-data arrives by CustomizeVM_Task as CloudinitPrep, and the mount point is derived
// from it while pending and after power-on applied it.
func TestVMDiskMountsCloudinitPrep(t *testing.T) {
	testFCD(t, func(e *fcdEnv) {
		vm := e.vm(0)
		ctx := registryContext(e.model.Map())
		vm.Config.ExtraConfig = append(vm.Config.ExtraConfig,
			&types.OptionValue{Key: userdataKey, Value: "---"},
			&types.OptionValue{Key: userdataEncodingKey, Value: "gzip+base64"})

		cc, err := yaml.Marshal(map[string]any{"write_files": []map[string]any{
			{"path": machineAgentConf, "content": "apiVersion: cluster.kubernetes.vmware.com/v1alpha1\nkind: MachineConfig\nspec:\n  volumeMounts:\n  - mountPoint: /var/lib/etcd\n    pcidevice: {bus: 3, target: 1}\n"},
		}})
		require.NoError(t, err)
		obj := object.NewVirtualMachine(e.c, vm.Self)
		require.NoError(t, e.wait(obj.Customize(e.ctx, types.CustomizationSpec{
			Identity: &types.CustomizationCloudinitPrep{Metadata: "instance-id: a", Userdata: string(cc)},
		})))

		unit := int32(1)
		require.NoError(t, e.attach(vm, e.createFCD("etcd"), 0, &unit))

		check := func(when string) {
			t.Helper()
			got, err := vm.diskMounts(ctx)
			require.NoError(t, err, when)
			require.Len(t, got, 1, when)
			require.Equal(t, "/var/lib/etcd", got[0].dest, when)
		}
		check("pending")
		require.NoError(t, e.wait(obj.PowerOn(e.ctx)))
		require.Nil(t, vm.imc, "power-on applied the customization")
		check("applied")
	})
}
