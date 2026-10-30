// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package simulator

import (
	"strings"
	"testing"

	"github.com/vmware/govmomi/toolbox"
	"github.com/vmware/govmomi/vim25/types"
)

// TestGuestNetDeviceConfigIDAgrees checks the container fallback for guest.net (FU-95113-124)
// reports the deviceConfigId the tools path does for the same guest NICs: the vNIC's device
// key matched by MAC, in any case, or -1 for a NIC with no vNIC. The control is the running
// index the fallback used before, which must disagree.
func TestGuestNetDeviceConfigIDAgrees(t *testing.T) {
	m := VPX()
	defer m.Remove()
	if err := m.Create(); err != nil {
		t.Fatal(err)
	}
	ctx := m.Service.Context
	vm := m.Map().Any("VirtualMachine").(*VirtualMachine)

	macs := []string{"00:11:22:33:44:55"} // no vNIC has it
	for _, d := range vm.Config.Hardware.Device {
		if card, ok := d.(types.BaseVirtualEthernetCard); ok {
			macs = append(macs, strings.ToUpper(card.GetVirtualEthernetCard().MacAddress))
		}
	}
	if len(macs) < 2 {
		t.Fatal("VM has no vNIC")
	}

	info := toolbox.NewGuestNicInfo()
	for _, mac := range macs {
		info.V3.Nics = append(info.V3.Nics, toolbox.GuestNicV3{MacAddress: mac})
	}
	ctx.WithLock(vm, func() { applyGuestNicInfo(ctx, vm, info.V3) })
	tools := make(map[string]int32, len(vm.Guest.Net))
	for _, nic := range vm.Guest.Net {
		tools[strings.ToLower(nic.MacAddress)] = nic.DeviceConfigId
	}

	tests := []struct {
		name     string
		fallback func(i int, mac string) int32
		agree    bool
	}{
		{"by MAC", func(_ int, mac string) int32 { return nicDeviceConfigID(vm, mac) }, true},
		{"control: running index", func(i int, _ string) int32 { return int32(i) }, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			agree := true
			for i, mac := range macs {
				want, ok := tools[strings.ToLower(mac)]
				if !ok {
					t.Fatalf("tools path dropped %s", mac)
				}
				if got := test.fallback(i, mac); got != want {
					agree = false
					if test.agree {
						t.Errorf("%s: deviceConfigId %d, tools path %d", mac, got, want)
					}
				}
			}
			if agree != test.agree {
				t.Errorf("fallback agrees with the tools path: %t, want %t", agree, test.agree)
			}
		})
	}
}
