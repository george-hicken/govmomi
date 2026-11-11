// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package simulator

import (
	"fmt"
	"log"
	"path"
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

// staleUnits returns the unit files in written that no disk of want has. Unit files are written
// to the container's filesystem, which a stop keeps, unlike its mounts.
func staleUnits(written []string, want []diskMount) []string {
	return slices.DeleteFunc(slices.Clone(written), func(u string) bool {
		return slices.ContainsFunc(want, func(m diskMount) bool { return m.unitPath() == u })
	})
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

// presentDisks makes the container's mounts the VM's current disk set, in the running container's
// mount namespace, so a disk attached or detached while it runs, or while it was stopped, reaches
// the guest without a power cycle. It is the one place that presents disks: at power-on, after the
// container's PID exists, and on each change. Only a disk's own directory enters the container.
//
// Mounts made this way last until the container stops; start presents the set again.
func (svm *simVM) presentDisks(ctx *Context) error {
	want, err := svm.vm.diskMounts(ctx)
	if err != nil {
		return err
	}
	add, remove := diskPlan(svm.diskLive, want)
	stale := staleUnits(svm.diskUnits, want)
	if len(add) == 0 && len(remove) == 0 && len(stale) == 0 {
		return nil
	}
	pid, err := svm.containerPID()
	if err != nil {
		return err
	}
	run := func(stdin string, args ...string) error {
		_, err := nsenterIn(pid, "-m", stdin, args...)
		return err
	}

	for _, m := range remove {
		if err := run("", "umount", m.dest); err != nil {
			return err
		}
	}
	for _, u := range stale {
		if err := run("", "rm", "-f", u); err != nil {
			return err
		}
	}
	for _, m := range add {
		if err := run("", "mkdir", "-p", m.dest, path.Dir(m.unitPath())); err != nil {
			return err
		}
		if err := mountTree(pid, m.source, m.dest); err != nil {
			return err
		}
		if err := run(m.mountUnit(), "tee", m.unitPath()); err != nil {
			return err
		}
	}
	svm.diskLive = want
	svm.diskUnits = nil
	for _, m := range want {
		svm.diskUnits = append(svm.diskUnits, m.unitPath())
	}

	// Units changed, so a running systemd must reload them; it then adopts the bind mounts.
	if _, err := svm.c.exec(ctx, []string{"test", "-d", "/run/systemd/system"}); err == nil {
		if _, err := svm.c.exec(ctx, []string{"systemctl", "daemon-reload"}); err != nil {
			log.Printf("%s: daemon-reload after disk change: %s", svm.vm.Name, err)
		}
	}
	return nil
}

// livePresent reports whether a disk change must reach the container now.
func (svm *simVM) livePresent() bool {
	return svm != nil && svm.c != nil && svm.c.id != "" &&
		svm.vm.Runtime.PowerState == types.VirtualMachinePowerStatePoweredOn
}
