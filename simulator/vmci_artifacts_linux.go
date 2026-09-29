// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package simulator

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
)

var (
	toolboxOnce sync.Once
	toolboxPath string
	toolboxErr  error
)

// buildToolbox builds the govmomi toolbox command for the host architecture,
// once per process, and returns the binary's path. A failed build is not
// retried within the process.
//
// Each process builds rather than reusing an earlier process's binary: Go's
// build cache makes an unchanged rebuild cheap, and a reused binary can
// predate a protocol change. The build runs in this package's source
// directory, so the binary matches the server it talks to.
func buildToolbox() (string, error) {
	toolboxOnce.Do(func() {
		toolboxPath, toolboxErr = goBuildToolbox()
		if toolboxErr == nil {
			log.Printf("vmci: built toolbox at %s", toolboxPath)
		}
	})
	return toolboxPath, toolboxErr
}

// goBuildToolbox writes the binary to bin/ beside this source file when that
// is writable, and to a temporary directory otherwise (a read-only module
// cache, or a binary built with -trimpath). It builds to a temporary name and
// renames, so a concurrent build never leaves a partly written file in place.
func goBuildToolbox() (string, error) {
	var srcDir string
	if _, file, _, ok := runtime.Caller(0); ok && filepath.IsAbs(file) {
		srcDir = filepath.Dir(file)
	}

	outDir := filepath.Join(srcDir, "bin")
	if srcDir == "" || os.MkdirAll(outDir, 0o755) != nil {
		dir, err := os.MkdirTemp("", "vcsim-toolbox-")
		if err != nil {
			return "", fmt.Errorf("toolbox build: %w", err)
		}
		outDir = dir
	}

	tmp, err := os.CreateTemp(outDir, "toolbox-*")
	if err != nil {
		return "", fmt.Errorf("toolbox build: %w", err)
	}
	_ = tmp.Close()
	defer os.Remove(tmp.Name())

	cmd := exec.Command("go", "build", "-o", tmp.Name(), "github.com/vmware/govmomi/toolbox/toolbox")
	cmd.Dir = srcDir
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+runtime.GOARCH)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("toolbox build: %w\n%s", err, out)
	}

	path := filepath.Join(outDir, "toolbox")
	if err := os.Rename(tmp.Name(), path); err != nil {
		return "", fmt.Errorf("toolbox build: %w", err)
	}
	return path, nil
}

// VmciToolboxBinaryPath returns the host path of the toolbox binary that vcsim
// mounts at /usr/bin/vmware-rpctool in container-backed VMs with RUN.vmci=true,
// building it on first use. It returns "" if the build failed.
//
// A caller whose guest runs vmtoolsd can mount the same binary in its place:
//
//	"RUN.volume.vmtoolsd": simulator.VmciToolboxBinaryPath() + ":/usr/bin/vmtoolsd:ro"
func VmciToolboxBinaryPath() string {
	path, _ := buildToolbox()
	return path
}
