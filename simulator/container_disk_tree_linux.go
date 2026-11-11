// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package simulator

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"

	"golang.org/x/sys/unix"
)

// diskTreeHelperEnv makes a vcsim process run as the mount helper of mountTree instead of its
// own main: setns(CLONE_NEWNS) is refused in a multithreaded process, which Go always is, and
// the mount must be made from podman's rootless namespaces, which podman unshare provides.
const diskTreeHelperEnv = "VCSIM_DISK_TREE_HELPER"

func init() {
	if os.Getenv(diskTreeHelperEnv) == "" {
		return
	}
	if err := diskTreeHelper(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

// mountTree makes the directory src visible at dest in the mount namespace of the container
// process pid, and nothing else of src's filesystem. src needs not be visible to the
// container, so it may be created after the container.
func mountTree(pid int, src, dest string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command("podman", "unshare", self, strconv.Itoa(pid), src, dest)
	cmd.Env = append(os.Environ(), diskTreeHelperEnv+"=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("mount %s at %s of pid %d: %w: %s", src, dest, pid, err, out)
	}
	return nil
}

// diskTreeHelper clones the tree at args[1] and moves it to args[2] in the mount namespace of
// the process args[0]. It runs on a thread with its own fs_struct, which setns requires.
func diskTreeHelper(args []string) error {
	if len(args) != 3 {
		return fmt.Errorf("usage: pid src dest")
	}
	runtime.LockOSThread()
	if err := unix.Unshare(unix.CLONE_FS); err != nil {
		return fmt.Errorf("unshare fs: %w", err)
	}
	tree, err := unix.OpenTree(unix.AT_FDCWD, args[1], unix.OPEN_TREE_CLONE|unix.OPEN_TREE_CLOEXEC|unix.AT_RECURSIVE)
	if err != nil {
		return fmt.Errorf("open_tree %s: %w", args[1], err)
	}
	ns, err := unix.Open("/proc/"+args[0]+"/ns/mnt", unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	if err := unix.Setns(ns, unix.CLONE_NEWNS); err != nil {
		return fmt.Errorf("setns: %w", err)
	}
	if err := unix.MoveMount(tree, "", unix.AT_FDCWD, args[2], unix.MOVE_MOUNT_F_EMPTY_PATH); err != nil {
		return fmt.Errorf("move_mount %s: %w", args[2], err)
	}
	return nil
}
