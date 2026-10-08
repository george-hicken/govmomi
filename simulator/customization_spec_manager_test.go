// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package simulator

import (
	"context"
	"testing"

	"github.com/vmware/govmomi/vim25"
	"github.com/vmware/govmomi/vim25/methods"
	"github.com/vmware/govmomi/vim25/types"
)

func TestIsGuestOsCustomizable(t *testing.T) {
	tests := map[string]bool{
		string(types.VirtualMachineGuestOsIdentifierVmwarePhoton64Guest):    true,
		string(types.VirtualMachineGuestOsIdentifierUbuntu64Guest):          true,
		string(types.VirtualMachineGuestOsIdentifierRhel9_64Guest):          true,
		string(types.VirtualMachineGuestOsIdentifierOracleLinux9_64Guest):   true,
		string(types.VirtualMachineGuestOsIdentifierWindows2019srv_64Guest): true,
		string(types.VirtualMachineGuestOsIdentifierWinXPProGuest):          true,
		string(types.VirtualMachineGuestOsIdentifierWin98Guest):             false,
		string(types.VirtualMachineGuestOsIdentifierOtherGuest):             false,
		string(types.VirtualMachineGuestOsIdentifierOther5xLinux64Guest):    false,
		string(types.VirtualMachineGuestOsIdentifierDarwin20_64Guest):       false,
		string(types.VirtualMachineGuestOsIdentifierSolaris11_64Guest):      false,
		string(types.VirtualMachineGuestOsIdentifierFreebsd13_64Guest):      false,
		"":           false,
		"bogusGuest": false,
	}

	Test(func(ctx context.Context, c *vim25.Client) {
		for id, expect := range tests {
			res, err := methods.IsGuestOsCustomizable(ctx, c, &types.IsGuestOsCustomizable{
				This:    *c.ServiceContent.CustomizationSpecManager,
				GuestId: id,
			})
			if err != nil {
				t.Fatalf("%q: %s", id, err)
			}
			if res.Returnval != expect {
				t.Errorf("%q: got %t, want %t", id, res.Returnval, expect)
			}
		}
	})
}
