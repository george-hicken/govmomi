// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package simulator

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/vim25/types"
)

// The guest paths a container VM's PVSCSI hardware appears at. The guest finds a disk as
// machine-agent does: the first sysfs PCI device with the PVSCSI vendor and device IDs, then
// /dev/disk/by-path/pci-<address>-scsi-0:0:<unit>:0. Rootless podman cannot make block devices,
// so the by-path name is the disk's content directory.
const (
	guestPCIDevicesDir = "/sys/bus/pci/devices"
	guestDiskByPathDir = "/dev/disk/by-path"
	pciVendorVMware    = "0x15ad"
	pciDevicePVSCSI    = "0x07c0"
)

// diskMount is one host directory presented at a guest path.
type diskMount struct {
	source string // host directory
	dest   string // guest path
}

// pvscsiDefaultSlots are the PCI slot numbers ESX gives PVSCSI controllers 0-3 that the spec
// leaves unplaced (inference: ESX defaults for scsi0-3; scsi0 at 160 is the documented one).
var pvscsiDefaultSlots = []int32{160, 224, 256, 288}

// placePVSCSI gives c a PCI slot, as ESX does, when the spec sets none: the first default slot
// no other controller of devices holds.
func placePVSCSI(devices object.VirtualDeviceList, c *types.ParaVirtualSCSIController) {
	if c.SlotInfo != nil {
		return
	}
	var used []int32
	for _, d := range devices.SelectByType((*types.ParaVirtualSCSIController)(nil)) {
		if s, ok := d.GetVirtualDevice().SlotInfo.(*types.VirtualDevicePciBusSlotInfo); ok && d.GetVirtualDevice().Key != c.Key {
			used = append(used, s.PciSlotNumber)
		}
	}
	for _, slot := range pvscsiDefaultSlots {
		if !slices.Contains(used, slot) {
			c.SlotInfo = &types.VirtualDevicePciBusSlotInfo{PciSlotNumber: slot}
			return
		}
	}
}

// guestPCIAddress returns the guest's PCI address (domain:bus:device.function) of a device in
// slot, by Broadcom KB 2047927: bits 0-4 are the device, bits 5-9 select the bridge (0 for the
// root bus, else pciBridge<n-1>), bits 10-12 the function.
func guestPCIAddress(slot int32) (string, error) {
	dev, sel, fn := slot&0x1f, (slot>>5)&0x1f, (slot>>10)&0x7
	if sel == 0 {
		return fmt.Sprintf("0000:00:%02x.%x", dev, fn), nil
	}
	bus, err := bridgeSecondaryBus(sel-1, fn)
	if err != nil {
		return "", fmt.Errorf("slot %d: %w", slot, err)
	}
	return fmt.Sprintf("0000:%02x:%02x.0", bus, dev), nil
}

// bridgeSecondaryBus returns the guest bus number behind function fn of pciBridge<bridge>. The KB
// does not give it; it is the guest firmware's enumeration, so this is the one place to correct
// against a real VM. pciBridge4-7 are PCIe root ports of 8 functions each, numbered from bus 3:
// pciBridge4 function 0 (slot 160) is bus 3, pciBridge5 function 0 (slot 192) bus 0x0b, as an
// ESX VM with the default bridges enumerates them.
func bridgeSecondaryBus(bridge, fn int32) (int32, error) {
	if bridge < 4 || bridge > 7 {
		return 0, fmt.Errorf("no guest bus is known behind pciBridge%d", bridge)
	}
	return 3 + 8*(bridge-4) + fn, nil
}

// diskByPath returns the guest path of the disk at unit on the PVSCSI controller at addr, as
// machine-agent formats it.
func diskByPath(addr string, unit int32) string {
	return fmt.Sprintf("%s/pci-%s-scsi-0:0:%x:0", guestDiskByPathDir, addr, unit)
}

// guestPVSCSI returns the guest PCI address of each PVSCSI controller of devices, by key. A
// controller with no slot or an unmapped one is logged once and left out.
func (vm *VirtualMachine) guestPVSCSI(devices object.VirtualDeviceList) map[int32]string {
	addrs := map[int32]string{}
	for _, d := range devices.SelectByType((*types.ParaVirtualSCSIController)(nil)) {
		c := d.GetVirtualDevice()
		slot, ok := c.SlotInfo.(*types.VirtualDevicePciBusSlotInfo)
		if !ok {
			vm.noteDiskOnce(c.Key, "PVSCSI controller %d has no PCI slot, its disks are not presented", c.Key)
			continue
		}
		addr, err := guestPCIAddress(slot.PciSlotNumber)
		if err != nil {
			vm.noteDiskOnce(c.Key, "PVSCSI controller %d: %s, its disks are not presented", c.Key, err)
			continue
		}
		addrs[c.Key] = addr
	}
	return addrs
}

// diskMounts returns the guest's PVSCSI controller addresses and its disks on them, each at its
// by-path name and sorted by it, creating each content directory as needed.
func (vm *VirtualMachine) diskMounts(ctx *Context) (pvscsi []string, mounts []diskMount, err error) {
	devices := object.VirtualDeviceList(vm.Config.Hardware.Device)
	addrs := vm.guestPVSCSI(devices)
	for _, d := range devices.SelectByType((*types.VirtualDisk)(nil)) {
		disk := d.(*types.VirtualDisk)
		addr, ok := addrs[disk.ControllerKey]
		if !ok || disk.UnitNumber == nil {
			vm.noteDiskOnce(disk.Key, "disk %d is not on a presented PVSCSI controller, not presented", disk.Key)
			continue
		}
		dir, err := contentDir(ctx, disk)
		if err != nil {
			return nil, nil, fmt.Errorf("disk unit %d: %w", *disk.UnitNumber, err)
		}
		mounts = append(mounts, diskMount{source: dir, dest: diskByPath(addr, *disk.UnitNumber)})
	}
	for _, addr := range addrs {
		pvscsi = append(pvscsi, addr)
	}
	sort.Strings(pvscsi)
	sort.Slice(mounts, func(i, j int) bool { return mounts[i].dest < mounts[j].dest })
	return pvscsi, mounts, nil
}

// writePCIDevices makes dir hold exactly a sysfs entry, vendor and device, for each address.
func writePCIDevices(dir string, addrs []string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !slices.Contains(addrs, e.Name()) {
			if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
				return err
			}
		}
	}
	for _, addr := range addrs {
		entry := filepath.Join(dir, addr)
		if _, err := os.Stat(entry); err == nil {
			continue // an entry's content is fixed, and its read-only files cannot be rewritten
		}
		if err := os.Mkdir(entry, 0o755); err != nil {
			return err
		}
		for name, id := range map[string]string{"vendor": pciVendorVMware, "device": pciDevicePVSCSI} {
			if err := os.WriteFile(filepath.Join(entry, name), []byte(id+"\n"), 0o444); err != nil {
				return err
			}
		}
	}
	return nil
}

// contentDir returns, creating it, the directory that holds disk's content:
// the backing file's path with its extension replaced by ".d".
func contentDir(ctx *Context, disk *types.VirtualDisk) (string, error) {
	backing, ok := disk.Backing.(types.BaseVirtualDeviceFileBackingInfo)
	if !ok {
		return "", fmt.Errorf("backing has no file")
	}
	info := backing.GetVirtualDeviceFileBackingInfo()
	if info.Datastore == nil {
		return "", fmt.Errorf("backing has no datastore")
	}
	ds, ok := ctx.Map.Get(*info.Datastore).(*Datastore)
	if !ok {
		return "", fmt.Errorf("datastore %s not found", info.Datastore)
	}
	var p object.DatastorePath
	if !p.FromString(info.FileName) {
		return "", fmt.Errorf("bad backing file %q", info.FileName)
	}
	rel := strings.TrimSuffix(p.Path, filepath.Ext(p.Path)) + ".d"
	dir := ds.resolve(ctx, rel)
	return dir, os.MkdirAll(dir, 0o755)
}

// noteDiskOnce logs, once per device key, why a device's disks are not presented, so a run log shows it.
func (vm *VirtualMachine) noteDiskOnce(key int32, format string, args ...any) {
	if vm.diskNoted[key] {
		return
	}
	if vm.diskNoted == nil {
		vm.diskNoted = map[int32]bool{}
	}
	vm.diskNoted[key] = true
	log.Printf("%s disks: %s", vm.Name, fmt.Sprintf(format, args...))
}
