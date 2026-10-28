// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package simulator

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/vmware/govmomi/vim25/types"
)

// RUN.virt values; see ContainerBackingOptionKey.
const (
	virtVMware    = "vmware"
	virtContainer = "container"
)

var errVirtPolicy = errors.New(`RUN.virt must be "vmware" or "container"`)

// parseVirt reports whether value asks the guest to see a VMware VM.
func parseVirt(value string) (bool, error) {
	switch value {
	case virtVMware:
		return true, nil
	case virtContainer:
		return false, nil
	}
	return false, fmt.Errorf("%w, not %q", errVirtPolicy, value)
}

// detectVirtPath is where vcsim mounts detectVirtScript for RUN.virt=vmware.
const detectVirtPath = "/usr/bin/systemd-detect-virt"

// detectVirtScript answers as systemd-detect-virt(1) does in the guest of a
// VMware VM: "vmware" for a VM, and no container, chroot, user namespace or
// confidential VM.
const detectVirtScript = `#!/bin/sh
# systemd-detect-virt as the guest of a VMware VM sees it (vcsim RUN.virt=vmware).
quiet=
mode=any
for arg in "$@"; do
	case "$arg" in
	-q | --quiet) quiet=1 ;;
	-v | --vm) mode=vm ;;
	-c | --container) mode=container ;;
	-r | --chroot) mode=chroot ;;
	--private-users) mode=private-users ;;
	--cvm) mode=cvm ;;
	--list) echo vmware; exit 0 ;;
	--list-cvm) exit 0 ;;
	esac
done
case "$mode" in
any | vm)
	[ -n "$quiet" ] || echo vmware
	exit 0
	;;
container | cvm)
	[ -n "$quiet" ] || echo none
	exit 1
	;;
esac
exit 1
`

// detectVirtShim returns a host file holding detectVirtScript, writing it if
// it is missing. The file is named by its content, so every vcsim process
// shares one, and it is replaced by rename, so a container never mounts a
// partly written file. It is checked at every start, as a temporary directory
// may be cleaned while a powered-off VM still names it.
func detectVirtShim() (string, error) {
	sum := sha256.Sum256([]byte(detectVirtScript))
	file := filepath.Join(os.TempDir(), "vcsim-systemd-detect-virt-"+hex.EncodeToString(sum[:6]))
	if _, err := os.Stat(file); err == nil {
		return file, nil
	}

	tmp, err := os.CreateTemp(os.TempDir(), "vcsim-systemd-detect-virt-*")
	if err != nil {
		return "", fmt.Errorf("systemd-detect-virt: %w", err)
	}
	defer os.Remove(tmp.Name())

	_, err = tmp.WriteString(detectVirtScript)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp.Name(), 0755)
	}
	if err == nil {
		err = os.Rename(tmp.Name(), file)
	}
	if err != nil {
		return "", fmt.Errorf("systemd-detect-virt: %w", err)
	}

	return file, nil
}

// virtMounts returns the mount of the detect-virt shim for RUN.virt=vmware,
// unless a RUN.volume already targets detectVirtPath.
func virtMounts(extraConfig []types.BaseOptionValue) ([]string, error) {
	if hasVolumeDest(extraConfig, detectVirtPath) {
		return nil, nil
	}
	file, err := detectVirtShim()
	if err != nil {
		return nil, err
	}
	return []string{file + ":" + detectVirtPath + ":ro"}, nil
}

// virtCreateArgs returns the create flags for RUN.virt=vmware. Podman puts
// container=podman in the environment of the container's PID 1, which
// cloud-init reads from /proc/1/environ to decide it runs in a container, and
// then ignores DMI. Without it, systemd as PID 1 still finds /run/.containerenv
// and runs as in a container. Docker sets no such variable, and a RUN.env.container
// key is the user's to keep.
func virtCreateArgs(opts createOptions) []string {
	if !opts.VMwareVirt || !runtimeNetwork().podman {
		return nil
	}
	for _, env := range opts.Env {
		if strings.HasPrefix(env, "container=") {
			return nil
		}
	}
	return []string{"--unsetenv", "container"}
}
