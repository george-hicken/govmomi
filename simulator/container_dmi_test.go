// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package simulator

import (
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// TestProductUUID pins the DMI product_uuid format: the dashed, uppercase form
// that the Linux kernel prints for /sys/class/dmi/id/product_uuid, with the
// first three fields byte-swapped from config.uuid, as SMBIOS stores them
// little-endian. The machine-id carrier is not this format; see
// TestMachineIDIsBIOSUUIDWithoutDashes.
func TestProductUUID(t *testing.T) {
	id := uuid.MustParse("93730430-037d-485e-9b48-e83fe8139686")
	require.Equal(t, "30047393-7D03-5E48-9B48-E83FE8139686", productUUID(id))
}

// TestDMIProductName pins product_name to the strings ESXi presents and checks
// that each passes cloud-init's DataSourceVMware.is_vmware_platform() test:
// "vmware" in system-product-name, case-insensitive.
func TestDMIProductName(t *testing.T) {
	tests := []struct{ firmware, version, want string }{
		{"efi", "vmx-20", "VMware20,1"},
		{"efi", "vmx-7", "VMware7,1"},
		{"efi", "", "VMware7,1"},
		{"bios", "vmx-20", "VMware Virtual Platform"},
		{"", "", "VMware Virtual Platform"},
	}
	for _, tt := range tests {
		got := dmiProductName(tt.firmware, tt.version)
		require.Equal(t, tt.want, got)
		require.Contains(t, strings.ToLower(got), "vmware")
	}
}
