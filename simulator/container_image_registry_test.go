// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package simulator

import (
	"testing"

	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
)

func testVMNamed(name string) *VirtualMachine {
	return &VirtualMachine{VirtualMachine: mo.VirtualMachine{Config: &types.VirtualMachineConfigInfo{Name: name}}}
}

func TestContainerImageRegistryResolve(t *testing.T) {
	var reg ContainerImageRegistry
	reg.Register(ContainerImageEntry{VMNameGlob: "example-bootstrap-vm", OCIImage: "bootstrap:latest"})
	reg.Register(ContainerImageEntry{VMNameGlob: "example-?????-?????", OCIImage: "node:latest"})

	tests := []struct {
		name string
		want string
	}{
		{"example-bootstrap-vm", "bootstrap:latest"},
		{"example-vpq4g-t4sr2", "node:latest"},
		{"example-vpq4g", ""},
		{"other-vpq4g-t4sr2", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := reg.resolve(testVMNamed(tc.name)).OCIImage; got != tc.want {
				t.Errorf("resolve(%q) = %q, want %q", tc.name, got, tc.want)
			}
		})
	}
}

func TestContainerImageEntryExtraConfigFor(t *testing.T) {
	e := ContainerImageEntry{ExtraConfig: map[string]string{
		"RUN.volume.varlog": "/logs/{vmname}:/var/log",
		"RUN.mountdmi":      "false",
	}}
	got := map[string]string{}
	for _, opt := range e.extraConfigFor("example-vpq4g-t4sr2") {
		ov := opt.(*types.OptionValue)
		got[ov.Key] = ov.Value.(string)
	}
	want := map[string]string{
		"RUN.volume.varlog": "/logs/example-vpq4g-t4sr2:/var/log",
		"RUN.mountdmi":      "false",
	}
	if len(got) != len(want) {
		t.Fatalf("extraConfigFor() = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("extraConfigFor()[%q] = %q, want %q", k, got[k], v)
		}
	}
}
