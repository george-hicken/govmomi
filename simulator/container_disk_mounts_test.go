// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package simulator

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/test"
	"github.com/vmware/govmomi/vim25/methods"
	"github.com/vmware/govmomi/vim25/types"
)

func TestGuestPCIAddress(t *testing.T) {
	for slot, want := range map[int32]string{
		16:           "0000:00:10.0", // root bus
		160:          "0000:03:00.0", // pciBridge4, the first PVSCSI
		192:          "0000:0b:00.0", // pciBridge5
		224:          "0000:13:00.0", // pciBridge6
		256:          "0000:1b:00.0", // pciBridge7
		1<<10 | 160:  "0000:04:00.0", // pciBridge4 function 1
		160 + 3:      "0000:03:03.0",
		2<<10 | 6<<5: "0000:0d:00.0", // pciBridge5 function 2
	} {
		got, err := guestPCIAddress(slot)
		require.NoError(t, err, slot)
		require.Equal(t, want, got, slot)
	}
	_, err := guestPCIAddress(1 << 5)
	require.Error(t, err, "no guest bus is known behind pciBridge0")
}

func TestDiskByPath(t *testing.T) {
	require.Equal(t, "/dev/disk/by-path/pci-0000:03:00.0-scsi-0:0:1:0", diskByPath("0000:03:00.0", 1))
	require.Equal(t, "/dev/disk/by-path/pci-0000:03:00.0-scsi-0:0:f:0", diskByPath("0000:03:00.0", 15), "unit in hex")
}

// TestPlacePVSCSI: a PVSCSI controller added without a slot gets the first free ESX default; a
// slot the spec sets is kept.
func TestPlacePVSCSI(t *testing.T) {
	testFCD(t, func(e *fcdEnv) {
		vm := e.vm(0)
		ctx := registryContext(e.model.Map())
		add := func(slot types.BaseVirtualDeviceBusSlotInfo) {
			t.Helper()
			ctrl, err := object.VirtualDeviceList(nil).CreateSCSIController("pvscsi")
			require.NoError(t, err)
			ctrl.GetVirtualDevice().SlotInfo = slot
			ctx.WithLock(vm, func() {
				require.Nil(t, vm.configureDevices(ctx, &types.VirtualMachineConfigSpec{DeviceChange: []types.BaseVirtualDeviceConfigSpec{
					&types.VirtualDeviceConfigSpec{Operation: types.VirtualDeviceConfigSpecOperationAdd, Device: ctrl},
				}}))
			})
		}
		slots := func() []int32 {
			var s []int32
			for _, d := range object.VirtualDeviceList(vm.Config.Hardware.Device).SelectByType((*types.ParaVirtualSCSIController)(nil)) {
				s = append(s, d.GetVirtualDevice().SlotInfo.(*types.VirtualDevicePciBusSlotInfo).PciSlotNumber)
			}
			return s
		}
		base := slots()
		require.Equal(t, pvscsiDefaultSlots[:len(base)], base, "the model's PVSCSI controllers are placed as ESX places them")
		add(&types.VirtualDevicePciBusSlotInfo{PciSlotNumber: 1184})
		add(nil)
		require.Equal(t, append(base, 1184, pvscsiDefaultSlots[len(base)]), slots())
	})
}

// TestGuestPCIParent: a device's parent is the root port whose secondary bus it is on, and the
// root bus has none.
func TestGuestPCIParent(t *testing.T) {
	for addr, want := range map[string]string{
		"0000:00:10.0": "",
		"0000:03:00.0": "0000:00:15.0",
		"0000:04:00.0": "0000:00:15.1",
		"0000:0b:00.0": "0000:00:16.0",
		"0000:0d:00.0": "0000:00:16.2",
		"0000:1b:00.0": "0000:00:18.0",
	} {
		got, err := guestPCIParent(addr)
		require.NoError(t, err, addr)
		require.Equal(t, want, got, addr)
	}
	_, err := guestPCIParent("0000:01:00.0")
	require.Error(t, err, "no guest bridge leads to bus 1")
	require.Equal(t, "../../../devices/pci0000:00", guestPCIRelLink)
	rel, err := filepath.Rel(guestPCIDevicesDir, guestPCIRootDir)
	require.NoError(t, err)
	require.Equal(t, rel, guestPCIRelLink)
}

// dirNames returns the names of the entries of dir, sorted.
func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// TestWritePCIDevices: each device is a symlink in the bus's device list to its directory under
// the root bridge, as sysfs lays it out, with vendor and device readable through the link; a device
// that goes is removed from both places, along with a bridge it leaves empty.
func TestWritePCIDevices(t *testing.T) {
	sys := t.TempDir()
	links := filepath.Join(sys, "bus", "pci", "devices")
	root := filepath.Join(sys, "devices", "pci0000:00")
	require.NoError(t, writePCIDevices(sys, []string{"0000:03:00.0", "0000:04:00.0", "0000:00:10.0"}))
	for addr, want := range map[string]string{
		"0000:03:00.0": "../../../devices/pci0000:00/0000:00:15.0/0000:03:00.0",
		"0000:04:00.0": "../../../devices/pci0000:00/0000:00:15.1/0000:04:00.0",
		"0000:00:10.0": "../../../devices/pci0000:00/0000:00:10.0",
	} {
		info, err := os.Lstat(filepath.Join(links, addr))
		require.NoError(t, err, addr)
		require.NotZero(t, info.Mode()&os.ModeSymlink, addr, "an entry is a symlink, as sysfs's is")
		got, err := os.Readlink(filepath.Join(links, addr))
		require.NoError(t, err)
		require.Equal(t, want, got, addr)
	}
	requirePCIDevices(t, sys, []string{"0000:03:00.0", "0000:04:00.0", "0000:00:10.0"})

	require.NoError(t, writePCIDevices(sys, []string{"0000:03:00.0"}))
	requirePCIDevices(t, sys, []string{"0000:03:00.0"})
	require.Equal(t, []string{"0000:03:00.0"}, dirNames(t, links))
	require.Equal(t, []string{"0000:00:15.0"}, dirNames(t, root), "the gone devices' bridges are removed with them")
	require.Equal(t, []string{"0000:03:00.0"}, dirNames(t, filepath.Join(root, "0000:00:15.0")))
}

// TestWritePCIDevicesRewrite: every device stays readable through its link after each write of a
// sequence, as presentDisks writes the tree again on every disk change while the guest runs.
func TestWritePCIDevicesRewrite(t *testing.T) {
	sys := t.TempDir()
	for _, addrs := range [][]string{
		{"0000:03:00.0"},
		{"0000:03:00.0"},
		{"0000:03:00.0", "0000:04:00.0"},
		{"0000:04:00.0"},
		{"0000:03:00.0", "0000:04:00.0", "0000:00:10.0"},
	} {
		require.NoError(t, writePCIDevices(sys, addrs))
		requirePCIDevices(t, sys, addrs)
	}
}

// requirePCIDevices requires that the bus's device list under sys is addrs, each readable as a
// PVSCSI controller through its link.
func requirePCIDevices(t *testing.T, sys string, addrs []string) {
	t.Helper()
	links := sysPath(sys, guestPCIDevicesDir)
	require.ElementsMatch(t, addrs, dirNames(t, links))
	for _, addr := range addrs {
		for name, id := range map[string]string{"vendor": "0x15ad\n", "device": "0x07c0\n"} {
			b, err := os.ReadFile(filepath.Join(links, addr, name))
			require.NoError(t, err, "%v: %s/%s", addrs, addr, name)
			require.Equal(t, id, string(b))
		}
	}
}

// machineAgentGetPCIDevice is machine-agent's GetPCIDevice (pkg/os/disk_linux.go) with the sysfs
// root as a parameter, so the walk the guest's disk lookup runs can run over a written tree. Its
// walk uses Lstat, so an entry that is a directory is skipped as not a device.
func machineAgentGetPCIDevice(sysRoot, vendorID, deviceID string) (string, error) {
	pciDevice := ""
	err := filepath.Walk(filepath.Join(sysRoot, "bus", "pci", "devices"), func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		vendor, device, err := machineAgentDeviceVendor(sysRoot, info.Name())
		if err != nil {
			return filepath.SkipDir
		}
		if vendorID == vendor && deviceID == device {
			pciDevice = info.Name()
			return filepath.SkipAll
		}
		return nil
	})
	return pciDevice, err
}

// machineAgentDeviceVendor is machine-agent's getDeviceVendor with the sysfs root as a parameter.
func machineAgentDeviceVendor(sysRoot, addr string) (vendorID, deviceID string, err error) {
	dir := filepath.Join(sysRoot, "bus", "pci", "devices", addr)
	data, err := os.ReadFile(filepath.Join(dir, "vendor"))
	if err != nil {
		return "", "", err
	}
	vendorID = strings.Trim(string(data), "\n")
	data, err = os.ReadFile(filepath.Join(dir, "device"))
	if err != nil {
		return "", "", err
	}
	return vendorID, strings.Trim(string(data), "\n"), nil
}

// TestWritePCIDevicesMachineAgentWalk: machine-agent's walk finds the PVSCSI controller at its
// address in the tree writePCIDevices writes.
func TestWritePCIDevicesMachineAgentWalk(t *testing.T) {
	sys := t.TempDir()
	require.NoError(t, writePCIDevices(sys, []string{"0000:03:00.0"}))
	addr, err := machineAgentGetPCIDevice(sys, pciVendorVMware, pciDevicePVSCSI)
	require.NoError(t, err)
	require.Equal(t, "0000:03:00.0", addr)
}

// TestWritePCIDevicesDirectoryLayoutFails is the control for the walk: the directory layout this
// replaces, with the same IDs, is not found, since the walk skips a directory as not a device.
func TestWritePCIDevicesDirectoryLayoutFails(t *testing.T) {
	sys := t.TempDir()
	dev := filepath.Join(sys, "bus", "pci", "devices", "0000:03:00.0")
	require.NoError(t, os.MkdirAll(dev, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dev, "vendor"), []byte(pciVendorVMware+"\n"), 0o444))
	require.NoError(t, os.WriteFile(filepath.Join(dev, "device"), []byte(pciDevicePVSCSI+"\n"), 0o444))
	addr, err := machineAgentGetPCIDevice(sys, pciVendorVMware, pciDevicePVSCSI)
	require.NoError(t, err)
	require.Empty(t, addr, "a directory entry is skipped, so the walk finds no device")
}

// TestVMDiskMounts: each disk on the PVSCSI controller is presented at its by-path name, whatever
// the guest's user-data says, and a detach removes it.
func TestVMDiskMounts(t *testing.T) {
	testFCD(t, func(e *fcdEnv) {
		vm := e.vm(0)
		ctx := registryContext(e.model.Map())

		pvscsi, base, err := vm.diskMounts(ctx)
		require.NoError(t, err)
		require.Equal(t, []string{"0000:03:00.0"}, pvscsi)
		require.Equal(t, []string{"/dev/disk/by-path/pci-0000:03:00.0-scsi-0:0:0:0"}, dests(base), "the VM's own disk")

		id := e.createFCD("etcd")
		other := e.createFCD("other")
		unit := int32(1)
		require.NoError(t, e.attach(vm, id, 0, &unit))
		unit = 10
		require.NoError(t, e.attach(vm, other, 0, &unit))

		dir := func(id types.ID) string {
			return strings.TrimSuffix(e.ds.resolve(ctx, strings.SplitN(e.fcdPath(id), "] ", 2)[1]), ".vmdk") + ".d"
		}
		_, got, err := vm.diskMounts(ctx)
		require.NoError(t, err)
		require.Equal(t, append(base,
			diskMount{source: dir(id), dest: "/dev/disk/by-path/pci-0000:03:00.0-scsi-0:0:1:0"},
			diskMount{source: dir(other), dest: "/dev/disk/by-path/pci-0000:03:00.0-scsi-0:0:a:0"},
		), got)
		require.DirExists(t, dir(id))

		require.NoError(t, e.detach(vm, id))
		_, got, err = vm.diskMounts(ctx)
		require.NoError(t, err)
		require.Equal(t, append(dests(base), "/dev/disk/by-path/pci-0000:03:00.0-scsi-0:0:a:0"), dests(got))
		require.DirExists(t, dir(id), "detach keeps the FCD's content")
	})
}

func dests(mounts []diskMount) []string {
	var d []string
	for _, m := range mounts {
		d = append(d, m.dest)
	}
	return d
}

const testDiskPath = "/dev/disk/by-path/pci-0000:03:00.0-scsi-0:0:1:0"

// TestContainerVM_DiskMount checks that a disk attached at unit 1 before power-on reaches the
// guest at its by-path name, with the PVSCSI controller in the guest's sysfs, and that detach
// while stopped removes it at the next power-on.
func TestContainerVM_DiskMount(t *testing.T) {
	env, vm, svm, id, attach := diskContainerSetup(t, "disk-mount")
	attach()

	powerOn(t, env, vm)
	cid := containerID(t, env, svm)
	requireGuestPVSCSI(t, env, cid)
	out, err := dockerExec(env.goCtx, cid, "readlink", guestPCIDevicesDir+"/0000:03:00.0")
	require.NoError(t, err)
	require.Equal(t, "../../../devices/pci0000:00/0000:00:15.0/0000:03:00.0", out, "an entry is a symlink into the device's directory, as in sysfs")

	_, err = dockerExec(env.goCtx, cid, "sh", "-c", "echo x > "+testDiskPath+"/probe")
	require.NoError(t, err)
	var dir string
	env.simCtx.WithLock(svm, func() { dir = svm.svm.diskLive[0].source })
	require.FileExists(t, filepath.Join(dir, "probe"))

	powerOff(t, env, vm)
	require.NoError(t, vm.DetachDisk(env.goCtx, id.Id))
	powerOn(t, env, vm)
	_, err = dockerExec(env.goCtx, containerID(t, env, svm), "test", "!", "-e", testDiskPath)
	require.NoError(t, err, "detach removes the disk at the next power-on")
	powerOff(t, env, vm)
}

// requireGuestPVSCSI requires that the guest's PCI devices are the VM's one PVSCSI controller,
// with vendor and device read through the guest's symlink.
func requireGuestPVSCSI(t *testing.T, env *vcSimEnv, cid string) {
	t.Helper()
	dev := guestPCIDevicesDir + "/0000:03:00.0"
	out, err := dockerExec(env.goCtx, cid, "sh", "-c", "ls "+guestPCIDevicesDir+"; cat "+dev+"/vendor "+dev+"/device")
	require.NoError(t, err)
	require.Equal(t, "0000:03:00.0\n0x15ad\n0x07c0", out, "the guest's PCI devices are the VM's")
}

// diskContainerSetup creates a container VM with a PVSCSI controller and an FCD ready; attach
// attaches the FCD at unit 1.
func diskContainerSetup(t *testing.T, name string) (env *vcSimEnv, vm *object.VirtualMachine, svm *VirtualMachine, id types.ID, attach func()) {
	t.Helper()
	test.RequireContainerOptIn(t)
	if !test.HasDocker() {
		t.Skip("requires docker or podman (aliased as docker) on linux")
	}

	env = newVCSIMEnv(t)
	vm, svm = createContainerVM(t, env, name, "")

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
// guest at once at its by-path name; after a stop it is presented again at the next start; a disk on a
// datastore made after the container is presented; and the guest sees nothing else of the datastore.
func TestContainerVM_DiskHotAttach(t *testing.T) {
	env, vm, svm, id, attach := diskContainerSetup(t, "disk-hot")

	powerOn(t, env, vm)
	cid := containerID(t, env, svm)
	_, err := dockerExec(env.goCtx, cid, "test", "!", "-e", testDiskPath)
	require.NoError(t, err, "no disk yet")

	present := func(cid string) {
		t.Helper()
		_, err := dockerExec(env.goCtx, cid, "sh", "-c", "echo x > "+testDiskPath+"/probe")
		require.NoError(t, err)
	}
	absent := func(cid string) {
		t.Helper()
		_, err := dockerExec(env.goCtx, cid, "test", "!", "-e", testDiskPath)
		require.NoError(t, err)
		_, err = dockerExec(env.goCtx, cid, "test", "!", "-e", "/var/lib/etcd/probe")
		require.NoError(t, err)
	}

	attach()
	present(cid)
	requireGuestPVSCSI(t, env, cid)
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
	out, err := dockerExec(env.goCtx, cid, "sh", "-c", "find / -name other-vm-secret 2>/dev/null; ls -d /.vcsim 2>/dev/null; ls "+testDiskPath)
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
