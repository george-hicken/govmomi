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
	conf := fmt.Sprintf("volumeMounts:\n- mountPoint: %s\n  pcidevice:\n    bus: %d\n    target: %d\n", mountPoint, bus, target)
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

func TestDiskMountPoints(t *testing.T) {
	for _, enc := range []string{"", "base64", "gzip+base64"} {
		got, err := diskMountPoints(userdataFor(t, enc, "/var/lib/etcd/", 3, 1))
		require.NoError(t, err, enc)
		require.Equal(t, map[scsiSlot]string{{3, 1}: "/var/lib/etcd"}, got, enc)
	}

	none, err := diskMountPoints(nil)
	require.NoError(t, err)
	require.Empty(t, none)
	none, err = diskMountPoints([]types.BaseOptionValue{&types.OptionValue{Key: userdataKey, Value: "runcmd: [true]\n"}})
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
		_, err := diskMountPoints(opts)
		require.Error(t, err, name)
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
// unit reaches the guest as its content directory with the guest's .mount
// unit, in place of the anonymous volume, and that detach undoes both at the
// next power-on.
func TestContainerVM_DiskMount(t *testing.T) {
	test.RequireContainerOptIn(t)
	if !test.HasDocker() {
		t.Skip("requires docker or podman (aliased as docker) on linux")
	}

	env := newVCSIMEnv(t)
	extra := append([]types.BaseOptionValue{
		&types.OptionValue{Key: "RUN.nestedContainers", Value: "true"},
		&types.OptionValue{Key: "RUN.nestedVolumes", Value: `["/var/lib/etcd"]`},
	}, userdataFor(t, "gzip+base64", "/var/lib/etcd", 0, 1)...)
	vm, svm := createContainerVM(t, env, "disk-mount", "", extra...)

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
