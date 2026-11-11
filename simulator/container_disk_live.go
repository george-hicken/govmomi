// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package simulator

import (
	"fmt"
	"os"
	"slices"
	"strconv"

	"github.com/vmware/govmomi/vim25/types"
)

// diskPlan returns the mounts to add and to remove to turn the presented set have into want.
// A mount point whose source changed is removed and added.
func diskPlan(have, want []diskMount) (add, remove []diskMount) {
	for _, m := range have {
		if !slices.Contains(want, m) {
			remove = append(remove, m)
		}
	}
	for _, m := range want {
		if !slices.Contains(have, m) {
			add = append(add, m)
		}
	}
	return add, remove
}

// containerPID returns the PID that owns the container's namespaces.
func (svm *simVM) containerPID() (int, error) {
	out, err := podmanOutput("inspect", "--format", "{{.State.Pid}}", svm.c.id)
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(out)
	if err != nil || pid == 0 {
		return 0, fmt.Errorf("%s: container %s has no PID %q", svm.vm.Name, svm.c.id, out)
	}
	return pid, nil
}

// presentDisks makes the container's guest hardware the VM's current PVSCSI controllers and disks,
// in the running container's mount namespace, so a disk attached or detached while it runs, or
// while it was stopped, reaches the guest without a power cycle. It is the one place that presents
// disks: at power-on, after the container's PID exists, and on each change. Only a disk's own
// directory enters the container.
//
// The controllers replace the container's view of the host's PCI devices, which are not the VM's.
// Mounts made this way last until the container stops; start presents the set again.
func (svm *simVM) presentDisks(ctx *Context) error {
	pvscsi, want, err := svm.vm.diskMounts(ctx)
	if err != nil {
		return err
	}
	if len(pvscsi) != 0 && svm.pciDir == "" {
		if svm.pciDir, err = os.MkdirTemp("", "vcsim-pci-"); err != nil {
			return err
		}
	}
	if svm.pciDir != "" {
		// The mount is of the directory, so later entries reach the guest through it.
		if err := writePCIDevices(svm.pciDir, pvscsi); err != nil {
			return err
		}
		want = append(want, diskMount{source: svm.pciDir, dest: guestPCIDevicesDir})
	}
	add, remove := diskPlan(svm.diskLive, want)
	if len(add) == 0 && len(remove) == 0 {
		return nil
	}
	pid, err := svm.containerPID()
	if err != nil {
		return err
	}
	run := func(args ...string) error {
		_, err := nsenterIn(pid, "-m", "", args...)
		return err
	}

	// Only by-path names are ever removed: pciDir, once mounted, stays for the container's life.
	for _, m := range remove {
		if err := run("umount", m.dest); err != nil {
			return err
		}
		if err := run("rmdir", m.dest); err != nil {
			return err
		}
	}
	for _, m := range add {
		if err := run("mkdir", "-p", m.dest); err != nil {
			return err
		}
		if err := mountTree(pid, m.source, m.dest); err != nil {
			return err
		}
	}
	svm.diskLive = want
	return nil
}

// removePCIDir removes the host directory of the guest's PCI devices.
func (svm *simVM) removePCIDir() {
	if svm.pciDir != "" {
		_ = os.RemoveAll(svm.pciDir)
		svm.pciDir = ""
	}
}

// livePresent reports whether a disk change must reach the container now.
func (svm *simVM) livePresent() bool {
	return svm != nil && svm.c != nil && svm.c.id != "" &&
		svm.vm.Runtime.PowerState == types.VirtualMachinePowerStatePoweredOn
}
