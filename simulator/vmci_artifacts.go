// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package simulator

import (
	"fmt"
	"os"
	"path/filepath"
)

// ToolboxEnv names the host path of a built github.com/vmware/govmomi/toolbox/toolbox
// binary, for GOOS=linux and the host architecture. vcsim never builds it: the
// caller builds it from the sources it runs and passes the path here.
const ToolboxEnv = "VCSIM_TOOLBOX"

// VmciToolboxBinary returns the host path of the toolbox binary that vcsim
// mounts at /usr/bin/vmware-rpctool in container-backed VMs with RUN.vmci=true,
// as given by ToolboxEnv. It refuses when ToolboxEnv is unset, relative, or not
// an executable file.
func VmciToolboxBinary() (string, error) {
	path := os.Getenv(ToolboxEnv)
	if path == "" {
		return "", fmt.Errorf("toolbox: %s is not set: give the host path of a built github.com/vmware/govmomi/toolbox/toolbox binary", ToolboxEnv)
	}
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("toolbox: %s=%q is not an absolute path", ToolboxEnv, path)
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("toolbox: %s: %w", ToolboxEnv, err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return "", fmt.Errorf("toolbox: %s=%q is not an executable file", ToolboxEnv, path)
	}
	return path, nil
}

// VmciToolboxBinaryPath returns the host path of the toolbox binary, as
// VmciToolboxBinary does, or "" if it refuses.
//
// A caller whose guest runs vmtoolsd can mount the same binary in its place:
//
//	"RUN.volume.vmtoolsd": simulator.VmciToolboxBinaryPath() + ":/usr/bin/vmtoolsd:ro"
func VmciToolboxBinaryPath() string {
	path, _ := VmciToolboxBinary()
	return path
}
