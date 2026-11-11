// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package simulator

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

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

func TestDiskMountPoints(t *testing.T) {
	opt := func(k, v string) types.BaseOptionValue { return &types.OptionValue{Key: k, Value: v} }

	got, err := diskMountPoints([]types.BaseOptionValue{opt("RUN.diskMount.0:1", "/var/lib/etcd/"), opt("RUN.container", "x")})
	require.NoError(t, err)
	require.Equal(t, map[string]string{"0:1": "/var/lib/etcd"}, got)

	for _, bad := range []types.BaseOptionValue{
		opt("RUN.diskMount.1", "/a"), opt("RUN.diskMount.x:1", "/a"), opt("RUN.diskMount.0:y", "/a"),
		opt("RUN.diskMount.0:1", "rel"), opt("RUN.diskMount.0:1", "/"),
	} {
		_, err := diskMountPoints([]types.BaseOptionValue{bad})
		require.Error(t, err, bad)
	}
}

func TestDiskMountVolumes(t *testing.T) {
	var units []string
	vols, err := diskMountVolumes([]diskMount{{source: "/d/x.d", dest: "/var/lib/etcd"}}, func(name, content string) (string, error) {
		units = append(units, name)
		require.Contains(t, content, "What=/d/x.d\n")
		require.Contains(t, content, "Where=/var/lib/etcd\n")
		return "/tmp/" + name, nil
	})
	require.NoError(t, err)
	require.Equal(t, []string{"var-lib-etcd.mount"}, units)
	require.Equal(t, []string{
		"/d/x.d:/var/lib/etcd",
		"/tmp/var-lib-etcd.mount:/etc/systemd/system/var-lib-etcd.mount:ro",
	}, vols)
}

// TestVMDiskMounts: an attached FCD at the named bus and unit is presented as
// its content directory, other units are not, and detach removes it.
func TestVMDiskMounts(t *testing.T) {
	testFCD(t, func(e *fcdEnv) {
		vm := e.vm(0)
		ctx := registryContext(e.model.Map())
		vm.Config.ExtraConfig = append(vm.Config.ExtraConfig,
			&types.OptionValue{Key: diskMountKeyPrefix + "0:1", Value: "/var/lib/etcd"})

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
// unit reaches the guest as its content directory with the guest's .mount
// unit, in place of the anonymous volume, and that detach undoes both at the
// next power-on.
func TestContainerVM_DiskMount(t *testing.T) {
	test.RequireContainerOptIn(t)
	if !test.HasDocker() {
		t.Skip("requires docker or podman (aliased as docker) on linux")
	}

	env := newVCSIMEnv(t)
	vm, svm := createContainerVM(t, env, "disk-mount", "",
		&types.OptionValue{Key: "RUN.nestedContainers", Value: "true"},
		&types.OptionValue{Key: "RUN.nestedVolumes", Value: `["/var/lib/etcd"]`},
		&types.OptionValue{Key: diskMountKeyPrefix + "0:1", Value: "/var/lib/etcd"})

	ctrl, err := object.VirtualDeviceList(nil).CreateSCSIController("pvscsi")
	require.NoError(t, err)
	task, err := vm.Reconfigure(env.goCtx, types.VirtualMachineConfigSpec{DeviceChange: []types.BaseVirtualDeviceConfigSpec{
		&types.VirtualDeviceConfigSpec{Operation: types.VirtualDeviceConfigSpecOperationAdd, Device: ctrl},
	}})
	require.NoError(t, err)
	require.NoError(t, task.Wait(env.goCtx))

	ds := env.simCtx.Map.Any("Datastore").(*Datastore)
	res, err := methods.CreateDisk_Task(env.goCtx, env.client.Client, &types.CreateDisk_Task{
		This: *env.client.ServiceContent.VStorageObjectManager,
		Spec: types.VslmCreateSpec{Name: "etcd", CapacityInMB: 10, BackingSpec: &types.VslmCreateSpecDiskFileBackingSpec{
			VslmCreateSpecBackingSpec: types.VslmCreateSpecBackingSpec{Datastore: ds.Self},
		}},
	})
	require.NoError(t, err)
	info, err := object.NewTask(env.client.Client, res.Returnval).WaitForResult(env.goCtx)
	require.NoError(t, err)
	id := info.Result.(types.VStorageObject).Config.Id
	unit := int32(1)
	require.NoError(t, vm.AttachDisk(env.goCtx, id.Id, object.NewDatastore(env.client.Client, ds.Self), 0, &unit))

	powerOn(t, env, vm)
	cid := containerID(t, env, svm)
	unitFile := "/etc/systemd/system/var-lib-etcd.mount"
	out, err := dockerExec(env.goCtx, cid, "cat", unitFile)
	require.NoError(t, err)
	require.Contains(t, out, "Where=/var/lib/etcd")

	_, err = dockerExec(env.goCtx, cid, "sh", "-c", "echo x > /var/lib/etcd/probe")
	require.NoError(t, err)
	var dir string
	env.simCtx.WithLock(svm, func() { dir = svm.svm.diskMounted[0].source })
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
