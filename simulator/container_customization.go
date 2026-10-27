// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package simulator

import (
	"archive/tar"
	"bytes"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/vmware/govmomi/vim25/types"
)

const (
	// imcDir is where VMware Tools' deployPkg plugin leaves a cloud-init
	// customization package, and where cloud-init's VMware datasource waits for
	// its cust.cfg (IMC_DIR_PATH in cloud-init's guestcust_util.py).
	imcDir = "/var/run/vmware-imc"

	// The names vpxd gives the CloudinitPrep documents in the package
	// (CLOUDINIT_METADATA_FILENAME and CLOUDINIT_USERDATA_FILENAME in
	// vpxd's gosc/vmCustomizer.cpp).
	imcMetadataFile = "metadata"
	imcUserdataFile = "userdata"
	imcConfigFile   = "cust.cfg"
)

// imcDeployScript writes the package the way Tools hands it to cloud-init: the
// documents first and cust.cfg last, by rename, because cloud-init starts
// reading as soon as cust.cfg exists.
var imcDeployScript = fmt.Sprintf(
	"umask 077 && mkdir -p %[1]s && tar -C %[1]s -xf - && mv -f %[1]s/.%[2]s %[1]s/%[2]s",
	imcDir, imcConfigFile)

// cloudinitPrepPackage returns the files of the deployPkg package vpxd builds
// for a CloudinitPrep customization: the metadata, the userdata if any, and a
// cust.cfg naming them, as CloudinitConfigGenerator::ToString writes it. vpxd
// trims both documents and drops an empty userdata. cust.cfg is named
// .cust.cfg here, for imcDeployScript to rename.
func cloudinitPrepPackage(prep *types.CustomizationCloudinitPrep, instanceID string) []tarEntry {
	var cfg strings.Builder
	cfg.WriteString("[CLOUDINIT]\n")
	cfg.WriteString("METADATA = " + imcMetadataFile + "\n")

	files := []tarEntry{
		{&tar.Header{Name: imcMetadataFile, Mode: 0600}, []byte(strings.TrimSpace(prep.Metadata))},
	}

	if userdata := strings.TrimSpace(prep.Userdata); userdata != "" {
		cfg.WriteString("USERDATA = " + imcUserdataFile + "\n")
		files = append(files, tarEntry{&tar.Header{Name: imcUserdataFile, Mode: 0600}, []byte(userdata)})
	}

	if instanceID != "" {
		cfg.WriteString("\n[MISC]\nINSTANCE-ID=" + instanceID + "\n")
	}

	return append(files, tarEntry{&tar.Header{Name: "." + imcConfigFile, Mode: 0600}, []byte(cfg.String())})
}

// deployCustomization delivers a pending customization to the guest of a
// container-backed VM, as Tools' deployPkg plugin does after power-on. Only
// CloudinitPrep is delivered: its package goes to imcDir, where cloud-init's
// VMware datasource reads it. Other identities have no guest-side effect. It
// is a no-op without a running container.
//
// The package is written into the running container rather than mounted or
// copied in before start: imcDir is under /run, a tmpfs that the runtime (or
// RUN.nestedContainers) mounts afresh at each start and that hides anything
// copied into the image, and cloud-init deletes imcDir once it has read the
// package, which a mount point would refuse. The guest needs sh, mkdir, tar and
// mv, as guest file upload needs tar.
func (svm *simVM) deployCustomization(spec *types.CustomizationSpec) error {
	if svm == nil || svm.c == nil || spec == nil {
		return nil
	}

	prep, ok := spec.Identity.(*types.CustomizationCloudinitPrep)
	if !ok {
		return nil
	}

	svm.c.Lock()
	id := svm.c.id
	svm.c.Unlock()
	if id == "" {
		return nil
	}

	var pkg bytes.Buffer
	tw := tar.NewWriter(&pkg)
	now := time.Now()
	for _, f := range cloudinitPrepPackage(prep, svm.vm.Config.InstanceUuid) {
		f.header.Size = int64(len(f.content))
		f.header.ModTime = now
		if err := tw.WriteHeader(f.header); err != nil {
			return err
		}
		if _, err := tw.Write(f.content); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}

	// As root, as Tools runs, whatever user the image runs as.
	cmd := exec.Command("docker", "exec", "-i", "-u", "0", id, "sh", "-c", imcDeployScript)
	cmd.Stdin = &pkg
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("deploy customization package to %s: %w: %s", imcDir, err, strings.TrimSpace(string(out)))
	}

	return nil
}
