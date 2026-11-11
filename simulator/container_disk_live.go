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
	"strings"

	"github.com/vmware/govmomi/vim25/types"
)

// diskStageDir is where a container sees each datastore's host directory, so that
// a disk attached while it runs can be bound from there: the source of a mount made
// in the container's namespace must be visible in it.
const diskStageDir = "/.vcsim/ds"

// diskStage is a datastore host directory staged at guest in the container.
type diskStage struct{ host, guest string }

// diskStages returns the datastore directories of ctx staged for a VM whose
// guest asks for disk mounts.
func diskStages(ctx *Context) []diskStage {
	var stages []diskStage
	for i, e := range ctx.Map.All("Datastore") {
		if ds, ok := e.(*Datastore); ok {
			stages = append(stages, diskStage{ds.Path(), path.Join(diskStageDir, strconv.Itoa(i))})
		}
	}
	return stages
}

func (s diskStage) volume() string { return s.host + ":" + s.guest }

// staged returns where the container sees source, a directory under one of stages.
func staged(stages []diskStage, source string) (string, bool) {
	for _, s := range stages {
		if rel, ok := strings.CutPrefix(source, strings.TrimSuffix(s.host, "/")+"/"); ok {
			return path.Join(s.guest, rel), true
		}
	}
	return "", false
}

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

// presentDisks makes the container's mounts the VM's current disk set, in the running container's
// mount namespace, so a disk attached or detached while it runs, or while it was stopped, reaches
// the guest without a power cycle. It is the one place that presents the set after create, where
// the set is passed as volumes instead.
//
// Mounts made this way last until the container stops; start presents the set again.
func (svm *simVM) presentDisks(ctx *Context) error {
	want, err := svm.vm.diskMounts(ctx)
	if err != nil {
		return err
	}
	add, remove := diskPlan(svm.diskLive, want)
	if len(add) == 0 && len(remove) == 0 {
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
		if slices.Contains(svm.diskMounted, m) {
			// The create-time unit is a bind mount of a host file.
			if err := run("", "umount", m.unitPath()); err != nil {
				return err
			}
		}
		if err := run("", "rm", "-f", m.unitPath()); err != nil {
			return err
		}
	}
	for _, m := range add {
		src, ok := staged(svm.diskStage, m.source)
		if !ok {
			return fmt.Errorf("disk %s is under no staged datastore", m.source)
		}
		if err := run("", "mkdir", "-p", m.dest, path.Dir(m.unitPath())); err != nil {
			return err
		}
		if err := run("", "mount", "--bind", src, m.dest); err != nil {
			return err
		}
		if err := run(m.mountUnit(), "tee", m.unitPath()); err != nil {
			return err
		}
	}
	svm.diskLive = want

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
	return svm != nil && svm.c != nil && svm.c.id != "" && svm.diskStage != nil &&
		svm.vm.Runtime.PowerState == types.VirtualMachinePowerStatePoweredOn
}
