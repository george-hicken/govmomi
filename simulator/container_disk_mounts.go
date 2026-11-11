// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package simulator

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/vim25/types"
)

// machineAgentConf is the cloud-config write_files path of the machine-agent
// configuration, the guest's own statement of where each disk is mounted. Only
// this file is read: it is the one product source of volumeMounts, and any
// other file's volumeMounts would not be an instruction to the guest.
const machineAgentConf = "/run/machine-agent/conf"

// The guestinfo keys carrying the cloud-config user-data and its encoding.
const (
	userdataKey         = "guestinfo.userdata"
	userdataEncodingKey = "guestinfo.userdata.encoding"
)

// diskMount is one disk presented to the guest.
type diskMount struct {
	source string // host directory
	dest   string // guest mount point
}

// systemdMountUnit returns the name of the .mount unit for mount point p, as
// systemd-escape --path --suffix=mount does.
func systemdMountUnit(p string) string {
	p = strings.Trim(path.Clean(p), "/")
	if p == "" {
		return "-.mount"
	}
	var b strings.Builder
	for i := 0; i < len(p); i++ {
		c := p[i]
		switch {
		case c == '/':
			b.WriteByte('-')
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == ':', c == '_',
			c == '.' && i > 0:
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, `\x%02x`, c)
		}
	}
	return b.String() + ".mount"
}

// mountUnit renders the unit machine-agent would write for m, a bind mount.
func (m diskMount) mountUnit() string {
	return fmt.Sprintf(`[Unit]
Description=Mount %[2]s

[Mount]
What=%[1]s
Where=%[2]s
Type=none
Options=bind

[Install]
WantedBy=local-fs.target
`, m.source, m.dest)
}

// unitPath is where the guest looks for m's unit.
func (m diskMount) unitPath() string {
	return "/etc/systemd/system/" + systemdMountUnit(m.dest)
}

// scsiSlot is a disk's place on a SCSI controller: the controller's bus number
// and the disk's unit number. machine-agent's volumeMounts name it as
// pcidevice{bus, target}.
type scsiSlot struct{ bus, unit int32 }

// mountPoint returns the mount point points names for the disk at unit, ignoring the guest's bus.
// This mirrors machine-agent (spec 9524 O-9524-15), which ignores pcidevice.Bus and resolves
// /dev/disk/by-path/pci-<PVSCSI address>-scsi-0:0:<target>:0 from the target alone; the kubernetes-service
// mutator's bus 3 against vm-operator's controllerBusNumber 0 is a cross-component misalignment it masks.
func mountPoint(points map[scsiSlot]string, unit int32) (string, bool) {
	for slot, dest := range points {
		if slot.unit == unit {
			return dest, true
		}
	}
	return "", false
}

// extraConfigValue returns the string value of key in extraConfig.
func extraConfigValue(extraConfig []types.BaseOptionValue, key string) string {
	for _, opt := range extraConfig {
		if v := opt.GetOptionValue(); v.Key == key {
			return fmt.Sprint(v.Value)
		}
	}
	return ""
}

// decodeUserdata returns the user-data text, decoded as encoding names:
// "base64", "gzip+base64" (also "gzip, base64"), or plain when empty.
func decodeUserdata(data, encoding string) ([]byte, error) {
	switch strings.ReplaceAll(strings.ToLower(strings.TrimSpace(encoding)), " ", "") {
	case "":
		return []byte(data), nil
	case "base64":
		return base64.StdEncoding.DecodeString(data)
	case "gzip+base64", "gzip,base64":
		raw, err := base64.StdEncoding.DecodeString(data)
		if err != nil {
			return nil, err
		}
		zr, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			return nil, err
		}
		defer zr.Close()
		return io.ReadAll(zr)
	}
	return nil, fmt.Errorf("unsupported encoding %q", encoding)
}

// diskMountPoints returns the mount point of each SCSI slot that the VM's
// guest user-data gives machine-agent in its volumeMounts. A VM without
// user-data, or whose user-data has no machine-agent configuration, has none.
func diskMountPoints(extraConfig []types.BaseOptionValue) (map[scsiSlot]string, error) {
	data := extraConfigValue(extraConfig, userdataKey)
	if data == "" {
		return nil, nil
	}
	text, err := decodeUserdata(data, extraConfigValue(extraConfig, userdataEncodingKey))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", userdataKey, err)
	}
	var cloudConfig struct {
		WriteFiles []struct {
			Path    string `yaml:"path"`
			Content string `yaml:"content"`
		} `yaml:"write_files"`
	}
	if err := yaml.Unmarshal(text, &cloudConfig); err != nil {
		return nil, fmt.Errorf("%s: %w", userdataKey, err)
	}

	points := map[scsiSlot]string{}
	for _, f := range cloudConfig.WriteFiles {
		if f.Path != machineAgentConf {
			continue
		}
		var conf struct {
			VolumeMounts []struct {
				MountPoint string `yaml:"mountPoint"`
				PCIDevice  *struct {
					Bus    int32 `yaml:"bus"`
					Target int32 `yaml:"target"`
				} `yaml:"pcidevice"`
			} `yaml:"volumeMounts"`
		}
		if err := yaml.Unmarshal([]byte(f.Content), &conf); err != nil {
			return nil, fmt.Errorf("%s: %w", machineAgentConf, err)
		}
		for _, m := range conf.VolumeMounts {
			if m.PCIDevice == nil {
				continue
			}
			if !path.IsAbs(m.MountPoint) || path.Clean(m.MountPoint) == "/" {
				return nil, fmt.Errorf("%s: %q is not an absolute mount point", machineAgentConf, m.MountPoint)
			}
			points[scsiSlot{m.PCIDevice.Bus, m.PCIDevice.Target}] = path.Clean(m.MountPoint)
		}
	}
	return points, nil
}

// diskMounts returns the disks of vm that diskMountPoints names, sorted by
// mount point, creating each content directory as needed.
func (vm *VirtualMachine) diskMounts(ctx *Context) ([]diskMount, error) {
	points, err := diskMountPoints(vm.Config.ExtraConfig)
	if err != nil || len(points) == 0 {
		return nil, err
	}

	devices := object.VirtualDeviceList(vm.Config.Hardware.Device)
	ctrlKey := pvscsiController(devices)
	var mounts []diskMount
	for _, d := range devices.SelectByType((*types.VirtualDisk)(nil)) {
		disk := d.(*types.VirtualDisk)
		if disk.ControllerKey != ctrlKey || disk.UnitNumber == nil {
			continue
		}
		dest, ok := mountPoint(points, *disk.UnitNumber)
		if !ok {
			continue
		}
		dir, err := contentDir(ctx, disk)
		if err != nil {
			return nil, fmt.Errorf("disk unit %d: %w", *disk.UnitNumber, err)
		}
		mounts = append(mounts, diskMount{source: dir, dest: dest})
	}
	sort.Slice(mounts, func(i, j int) bool { return mounts[i].dest < mounts[j].dest })
	return mounts, nil
}

// pvscsiController returns the key of the PVSCSI controller machine-agent resolves disks on, or 0.
// With several, the lowest bus number is taken (inference: the sim presents one PVSCSI address).
func pvscsiController(devices object.VirtualDeviceList) int32 {
	var key, bus int32 = 0, -1
	for _, d := range devices.SelectByType((*types.ParaVirtualSCSIController)(nil)) {
		c := d.(*types.ParaVirtualSCSIController)
		if bus < 0 || c.BusNumber < bus {
			key, bus = c.Key, c.BusNumber
		}
	}
	return key
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

// volumes returns the container volume arguments presenting ms: the disk
// directory at its mount point and its unit file, written by write.
func diskMountVolumes(ms []diskMount, write func(name, content string) (string, error)) ([]string, error) {
	var vols []string
	for _, m := range ms {
		unit, err := write(systemdMountUnit(m.dest), m.mountUnit())
		if err != nil {
			return nil, err
		}
		vols = append(vols, m.source+":"+m.dest, unit+":"+m.unitPath()+":ro")
	}
	return vols, nil
}
