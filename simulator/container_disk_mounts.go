// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package simulator

import (
	"fmt"
	"io/fs"
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
//
// machine-agent walks guestPCIDevicesDir and skips directories, so each entry there is a symlink to
// its device's directory under guestPCIRootDir, as sysfs makes it. The link is relative, so both
// directories are mounted in the guest at the paths the link reaches.
const (
	guestPCIDevicesDir = "/sys/bus/pci/devices"
	guestPCIRootDir    = "/sys/devices/pci0000:00"
	guestDiskByPathDir = "/dev/disk/by-path"
	pciVendorVMware    = "0x15ad"
	pciDevicePVSCSI    = "0x07c0"
)

// guestPCIRelLink is the path from guestPCIDevicesDir to guestPCIRootDir, which sysfs writes into
// each entry's symlink.
const guestPCIRelLink = "../../../devices/pci0000:00"

// sysPath returns the path under sysRoot, the host tree of the guest's /sys, of guest.
func sysPath(sysRoot, guest string) string {
	return filepath.Join(sysRoot, strings.TrimPrefix(guest, "/sys/"))
}

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

// guestPCIParent returns the guest address of the root port the device at addr sits behind, or ""
// for a device on the root bus. It is the inverse of bridgeSecondaryBus.
func guestPCIParent(addr string) (string, error) {
	var bus, dev, fn int32
	if _, err := fmt.Sscanf(addr, "0000:%x:%x.%x", &bus, &dev, &fn); err != nil {
		return "", fmt.Errorf("PCI address %q: %w", addr, err)
	}
	if bus == 0 {
		return "", nil
	}
	for bridge := int32(4); bridge <= 7; bridge++ {
		for f := int32(0); f < 8; f++ {
			if sec, _ := bridgeSecondaryBus(bridge, f); sec == bus {
				// pciBridge<n> is at 0000:00:<0x11+n>, ESX's default slot for it (inference, as above).
				return fmt.Sprintf("0000:00:%02x.%x", 0x11+bridge, f), nil
			}
		}
	}
	return "", fmt.Errorf("no guest bridge leads to bus %d", bus)
}

// writePCIDevices lays the guest's PCI devices out under sysRoot, the host tree of the guest's
// /sys, as the kernel does. guestPCIDevicesDir holds a symlink per address to the device's
// directory under guestPCIRootDir, and the device directory holds vendor and device. The bridge
// directories between them are bare: the VM's bridges are not modelled. A device that is gone is
// removed from both places, and an entry's content is fixed once written.
func writePCIDevices(sysRoot string, addrs []string) error {
	links, root := sysPath(sysRoot, guestPCIDevicesDir), sysPath(sysRoot, guestPCIRootDir)
	if err := os.MkdirAll(links, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	devDirs := make([]string, len(addrs))
	for i, addr := range addrs {
		parent, err := guestPCIParent(addr)
		if err != nil {
			return err
		}
		devDirs[i] = filepath.Join(parent, addr)
	}
	if err := pruneTree(links, addrs); err != nil {
		return err
	}
	if err := pruneTree(root, devDirs); err != nil {
		return err
	}
	for i, addr := range addrs {
		link := filepath.Join(links, addr)
		if _, err := os.Lstat(link); err == nil {
			continue // an entry's content is fixed, and its read-only files cannot be rewritten
		}
		dev := filepath.Join(root, devDirs[i])
		if err := writePCIDevice(dev); err != nil {
			return err
		}
		// The device directory is complete before its link, so no reader sees a dangling entry.
		if err := os.Symlink(filepath.Join(guestPCIRelLink, devDirs[i]), link); err != nil {
			return err
		}
	}
	return nil
}

// writePCIDevice makes dir a PCI device directory of a PVSCSI controller.
func writePCIDevice(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for name, id := range map[string]string{"vendor": pciVendorVMware, "device": pciDevicePVSCSI} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(id+"\n"), 0o444); err != nil {
			return err
		}
	}
	return nil
}

// pruneTree removes from root every entry that is not one of the slash-separated paths keep, nor
// an ancestor or descendant of one: a kept path's content is its own.
func pruneTree(root string, keep []string) error {
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil || rel == "." {
			return err
		}
		if slices.Contains(keep, rel) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if slices.ContainsFunc(keep, func(k string) bool { return strings.HasPrefix(k, rel+string(filepath.Separator)) }) {
			return nil
		}
		if err := os.RemoveAll(p); err != nil {
			return err
		}
		if d.IsDir() {
			return filepath.SkipDir
		}
		return nil // SkipDir on a file would skip its siblings
	})
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
