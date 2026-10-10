// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package simulator

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/vmware/govmomi/vim25/methods"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
)

func TestHostVirtualNicManagerSelectVnicForNicType(t *testing.T) {
	const nicType = "management"
	const vmk0 = "management.key-vim.host.VirtualNic-vmk0"

	selected := func(m *HostVirtualNicManager) []string {
		body := m.QueryNetConfig(&types.QueryNetConfig{NicType: nicType}).(*methods.QueryNetConfigBody)
		require.Nil(t, body.Fault())
		return body.Res.Returnval.SelectedVnic
	}

	m := NewHostVirtualNicManager(&mo.HostSystem{})
	require.Equal(t, []string{vmk0}, selected(m), "vmk0 is the management vNIC by default")

	body := m.DeselectVnicForNicType(&types.DeselectVnicForNicType{NicType: nicType, Device: "vmk0"}).(*methods.DeselectVnicForNicTypeBody)
	require.Nil(t, body.Fault())
	require.Empty(t, selected(m))

	body2 := m.SelectVnicForNicType(&types.SelectVnicForNicType{NicType: nicType, Device: "vmk0"}).(*methods.SelectVnicForNicTypeBody)
	require.Nil(t, body2.Fault())
	require.Equal(t, []string{vmk0}, selected(m))

	fault := m.SelectVnicForNicType(&types.SelectVnicForNicType{NicType: nicType, Device: "vmk9"}).(*methods.SelectVnicForNicTypeBody)
	require.NotNil(t, fault.Fault(), "a device that is not a candidate is refused")
	require.Equal(t, []string{vmk0}, selected(m))

	// Another host's selections are its own.
	other := NewHostVirtualNicManager(&mo.HostSystem{})
	m.DeselectVnicForNicType(&types.DeselectVnicForNicType{NicType: nicType, Device: "vmk0"})
	require.Equal(t, []string{vmk0}, selected(other))
}
