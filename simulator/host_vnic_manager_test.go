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

// TestHostVirtualNicManagerDefaultHost pins that every simulated host has vmk0 tagged for management, and that the
// host's own NetConfig and its vNIC manager agree, through selection changes.
func TestHostVirtualNicManagerDefaultHost(t *testing.T) {
	model := VPX()
	require.NoError(t, model.Create())
	defer model.Remove()
	ctx := model.Service.Context

	refs := ctx.Map.AllReference("HostSystem")
	require.NotEmpty(t, refs)
	for _, ref := range refs {
		host := ctx.Map.Get(ref.Reference()).(*HostSystem)
		m := ctx.Map.Get(host.ConfigManager.VirtualNicManager.Reference()).(*HostVirtualNicManager)
		const vmk0 = "management.key-vim.host.VirtualNic-vmk0"

		require.Equal(t, []string{vmk0}, hostSelected(host, "management"), "host %s", host.Name)
		body := m.QueryNetConfig(&types.QueryNetConfig{NicType: "management"}).(*methods.QueryNetConfigBody)
		require.Nil(t, body.Fault())
		require.Equal(t, []string{vmk0}, body.Res.Returnval.SelectedVnic)

		m.DeselectVnicForNicType(&types.DeselectVnicForNicType{NicType: "management", Device: "vmk0"})
		require.Empty(t, hostSelected(host, "management"), "the host sees the deselection")
		m.SelectVnicForNicType(&types.SelectVnicForNicType{NicType: "management", Device: "vmk0"})
		require.Equal(t, []string{vmk0}, hostSelected(host, "management"), "the host sees the selection")
	}
}

func hostSelected(host *HostSystem, nicType string) []string {
	for _, c := range host.Config.VirtualNicManagerInfo.NetConfig {
		if c.NicType == nicType {
			return c.SelectedVnic
		}
	}
	return nil
}

// TestManagementUnderlay pins that the topology decides which underlay vmnic0 is on: the one marked management when no
// attachment maps it, and the attachment's underlay otherwise.
func TestManagementUnderlay(t *testing.T) {
	top := &Topology{Underlays: []Underlay{{Name: "plain"}, {Name: "mgmt", Management: true}}}
	u, err := top.attachedUnderlay("esx0", "vmnic0")
	require.NoError(t, err)
	require.Equal(t, "mgmt", u, "vmnic0 defaults to the management underlay")

	top.Attachments = []Attachment{{Host: "*", Pnic: "vmnic0", Underlay: "plain"}}
	u, err = top.attachedUnderlay("esx0", "vmnic0")
	require.NoError(t, err)
	require.Equal(t, "plain", u, "an attachment overrides the management default")
}
