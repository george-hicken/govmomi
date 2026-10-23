// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package simulator

import (
	"context"
	"reflect"
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

// collidingIsGuestOsCustomizable stands in for a same-named request type that another
// package, such as govmomi-internal's vim25/types, registers over the govmomi one.
type collidingIsGuestOsCustomizable struct {
	This    types.ManagedObjectReference `xml:"_this"`
	GuestId string                       `xml:"guestId"`
}

func TestIsGuestOsCustomizableRegistryCollision(t *testing.T) {
	name := "IsGuestOsCustomizable"
	orig, ok := types.TypeFunc()(name)
	if !ok {
		t.Fatalf("%s is not registered", name)
	}
	types.Add(name, reflect.TypeOf((*collidingIsGuestOsCustomizable)(nil)).Elem())
	t.Cleanup(func() { types.Add(name, orig) })

	Test(func(ctx context.Context, c *vim25.Client) {
		res, err := methods.IsGuestOsCustomizable(ctx, c, &types.IsGuestOsCustomizable{
			This:    *c.ServiceContent.CustomizationSpecManager,
			GuestId: string(types.VirtualMachineGuestOsIdentifierVmwarePhoton64Guest),
		})
		if err != nil {
			t.Fatal(err)
		}
		if !res.Returnval {
			t.Error("vmwarePhoton64Guest: got false, want true")
		}
	})
}
