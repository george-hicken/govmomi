// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package simulator

import (
	"context"
	"slices"
	"testing"

	"github.com/vmware/govmomi/find"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/vim25"
	"github.com/vmware/govmomi/vim25/types"
)

// TestVirtualMachineNICLabelAndUnit checks vNICs added without a unit number get a PCI slot
// each and the "Network adapter N" label vSphere gives them. Before, a vNIC added with a MAC
// kept a nil unit number and every vNIC got the client-side name "ethernet--7".
func TestVirtualMachineNICLabelAndUnit(t *testing.T) {
	Test(func(ctx context.Context, c *vim25.Client) {
		finder := find.NewFinder(c)
		vm, err := finder.VirtualMachine(ctx, "DC0_H0_VM0")
		if err != nil {
			t.Fatal(err)
		}
		net, err := finder.Network(ctx, "VM Network")
		if err != nil {
			t.Fatal(err)
		}
		backing, err := net.EthernetCardBackingInfo(ctx)
		if err != nil {
			t.Fatal(err)
		}

		nics := func() object.VirtualDeviceList {
			devices, err := vm.Device(ctx)
			if err != nil {
				t.Fatal(err)
			}
			return devices.SelectByType((*types.VirtualEthernetCard)(nil))
		}
		add := func(mac string) {
			nic, err := object.EthernetCardTypes().CreateEthernetCard("vmxnet3", backing)
			if err != nil {
				t.Fatal(err)
			}
			card := nic.(types.BaseVirtualEthernetCard).GetVirtualEthernetCard()
			if mac != "" {
				card.AddressType = string(types.VirtualEthernetCardMacTypeManual)
				card.MacAddress = mac
			}
			if err = vm.AddDevice(ctx, nic); err != nil {
				t.Fatal(err)
			}
		}

		// The model VM has one vNIC; remove it so the labels start from scratch.
		if err = vm.RemoveDevice(ctx, false, nics()...); err != nil {
			t.Fatal(err)
		}
		add("00:50:56:00:00:01") // MAC given: the case that kept a nil unit number
		add("")
		add("")

		check := func(want ...string) {
			t.Helper()
			units := map[int32]bool{}
			var got []string
			for _, nic := range nics() {
				d := nic.GetVirtualDevice()
				if d.UnitNumber == nil {
					t.Fatalf("%s has no unit number", d.DeviceInfo.GetDescription().Label)
				}
				if units[*d.UnitNumber] {
					t.Errorf("unit number %d reused", *d.UnitNumber)
				}
				units[*d.UnitNumber] = true
				got = append(got, d.DeviceInfo.GetDescription().Label)
			}
			slices.Sort(got)
			slices.Sort(want)
			if len(got) != len(want) {
				t.Fatalf("labels %v, want %v", got, want)
			}
			for i := range want {
				if got[i] != want[i] {
					t.Errorf("labels %v, want %v", got, want)
					break
				}
			}
		}
		check("Network adapter 1", "Network adapter 2", "Network adapter 3")

		// A new vNIC takes the lowest free number (1); an edit keeps its label (2).
		if err = vm.RemoveDevice(ctx, false, nics()[0]); err != nil {
			t.Fatal(err)
		}
		edit := nics()[0]
		label := edit.GetVirtualDevice().DeviceInfo.GetDescription().Label
		edit.GetVirtualDevice().DeviceInfo = nil
		if err = vm.EditDevice(ctx, edit); err != nil {
			t.Fatal(err)
		}
		if got := nics().FindByKey(edit.GetVirtualDevice().Key).GetVirtualDevice().DeviceInfo.GetDescription().Label; got != label {
			t.Errorf("edit relabelled %q as %q", label, got)
		}
		add("")
		check("Network adapter 1", "Network adapter 2", "Network adapter 3")
	})
}
