// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package simulator

import (
	"fmt"
	"sort"
	"strings"

	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
)

// ethernetCards returns vm's virtual Ethernet cards in device-key order. The i-th card is the container's eth<i>.
func ethernetCards(vm *VirtualMachine) []*types.VirtualEthernetCard {
	var cards []*types.VirtualEthernetCard
	for _, d := range vm.Config.Hardware.Device {
		if eth, ok := d.(types.BaseVirtualEthernetCard); ok {
			cards = append(cards, eth.GetVirtualEthernetCard())
		}
	}
	sort.SliceStable(cards, func(i, j int) bool { return cards[i].Key < cards[j].Key })
	return cards
}

// nicNetwork returns the network a vNIC is backed by, in the form ResolveVNIC takes.
func nicNetwork(card *types.VirtualEthernetCard) (types.ManagedObjectReference, error) {
	switch b := card.Backing.(type) {
	case *types.VirtualEthernetCardDistributedVirtualPortBackingInfo:
		return types.ManagedObjectReference{Type: "DistributedVirtualPortgroup", Value: b.Port.PortgroupKey}, nil
	case *types.VirtualEthernetCardNetworkBackingInfo:
		if b.Network != nil {
			return *b.Network, nil
		}
	}
	return types.ManagedObjectReference{}, fmt.Errorf("nic %d: unsupported backing %T", card.Key, card.Backing)
}

// nicNetworkName is the vSphere name of the network a vNIC is backed by, or "" if it cannot be read.
func nicNetworkName(ctx *Context, card *types.VirtualEthernetCard) string {
	if ref, err := nicNetwork(card); err == nil {
		switch n := ctx.Map.Get(ref).(type) {
		case *DistributedVirtualPortgroup:
			return n.Name
		case *mo.Network:
			return n.Name
		case *mo.OpaqueNetwork:
			return n.Name
		}
	}
	if b, ok := card.Backing.(*types.VirtualEthernetCardNetworkBackingInfo); ok {
		return b.DeviceName
	}
	return ""
}

// nicConnected reports whether a vNIC is connected. A card with no connectable state is connected, as a real
// device is when created with the defaults.
func nicConnected(card *types.VirtualEthernetCard) bool {
	return card.Connectable == nil || card.Connectable.Connected
}

// live reports whether a vNIC change must reach the container now: the VM is powered on with a container, and a
// topology is loaded. Without a topology the container has no vNIC beyond its power-on network, as before.
func (svm *simVM) live(ctx *Context) bool {
	return svm != nil && svm.c != nil && svm.c.id != "" && ctx.Map.underlay != nil &&
		svm.vm.Runtime.PowerState == types.VirtualMachinePowerStatePoweredOn
}

// domainOf returns the L2 domain a vNIC is on, resolved on the VM's host through the topology.
func (svm *simVM) domainOf(ctx *Context, card *types.VirtualEthernetCard) (Domain, error) {
	if svm.vm.Runtime.Host == nil {
		return Domain{}, fmt.Errorf("%s: no host to resolve nic %d", svm.vm.Name, card.Key)
	}
	host, ok := ctx.Map.Get(*svm.vm.Runtime.Host).(*HostSystem)
	if !ok {
		return Domain{}, fmt.Errorf("%s: host %s not found", svm.vm.Name, svm.vm.Runtime.Host.Value)
	}
	ref, err := nicNetwork(card)
	if err != nil {
		return Domain{}, err
	}
	return ctx.Map.underlay.topo.ResolveVNIC(ctx, host, ref)
}

// nicState is a vNIC as the container sees it: whether it is joined to a podman network, which one, and its MAC.
type nicState struct {
	present  bool
	attached bool
	network  string
	mac      string
}

// nicStateOf returns the container-side state of card. A nil card is absent.
func (svm *simVM) nicStateOf(ctx *Context, card *types.VirtualEthernetCard) (nicState, error) {
	if card == nil {
		return nicState{}, nil
	}
	st := nicState{present: true, mac: card.MacAddress}
	if !nicConnected(card) {
		return st, nil
	}
	d, err := svm.domainOf(ctx, card)
	if err != nil {
		return st, err
	}
	name, err := ctx.Map.underlay.Network(d)
	if err != nil {
		return st, err
	}
	st.attached = true
	st.network = name
	return st, nil
}

// capAdd returns the capabilities a container VM needs for its guest networking: with a topology, NET_RAW for ping.
func capAdd(ctx *Context) []string {
	if ctx.Map.underlay == nil {
		return nil
	}
	return []string{"NET_RAW"}
}

// containerNICs returns the interfaces the container joins at power-on, in device-key order, and sets each
// vNIC's connected state from its startConnected flag. Without a topology it is the first vNIC's MAC on the
// default network, as before. With one, each connected vNIC joins its own domain, and two vNICs on one domain
// are refused because podman joins a container to a network once.
func (svm *simVM) containerNICs(ctx *Context) ([]containerNIC, error) {
	if ctx.Map.underlay == nil {
		return []containerNIC{{MAC: firstNICMacAddress(svm.vm)}}, nil
	}

	var nics []containerNIC
	domains := make(map[string]int32)
	for i, card := range ethernetCards(svm.vm) {
		if card.Connectable != nil {
			card.Connectable.Connected = card.Connectable.StartConnected
		}
		if !nicConnected(card) {
			continue
		}

		d, err := svm.domainOf(ctx, card)
		if err != nil {
			return nil, err
		}
		if other, dup := domains[d.Key()]; dup {
			return nil, fmt.Errorf("%s: nics %d and %d both join %s", svm.vm.Name, other, card.Key, d.Key())
		}
		domains[d.Key()] = card.Key

		name, err := ctx.Map.underlay.Network(d)
		if err != nil {
			return nil, err
		}
		nics = append(nics, containerNIC{Network: name, MAC: card.MacAddress, Name: fmt.Sprintf("eth%d", i)})
	}
	return nics, nil
}

// applyNIC moves a running container's interface from its before state to its after state: disconnect from
// the old network, connect to the new one with the same MAC. podman names the new interface itself, so the
// in-guest name can change on a move; guests find the interface by MAC.
func (svm *simVM) applyNIC(before, after nicState) error {
	moved := before.network != after.network || before.mac != after.mac
	if before.attached && (!after.attached || moved) {
		if _, err := podmanOutput("network", "disconnect", before.network, svm.c.id); err != nil {
			return err
		}
	}
	if after.attached && (!before.attached || moved) {
		args := []string{"network", "connect"}
		if after.mac != "" {
			args = append(args, "--mac-address", after.mac)
		}
		args = append(args, after.network, svm.c.id)
		if _, err := podmanOutput(args...); err != nil {
			return err
		}
	}
	return nil
}

// nicGuestInfo builds guest.net for a container VM with a topology: one entry per vNIC in device-key order, found
// in the container by MAC, with an address only when the container reports one. It also returns the first
// connected vNIC's container settings, which give the VM's primary address and gateway.
func (svm *simVM) nicGuestInfo(ctx *Context, networks map[string]networkSettings) ([]types.GuestNicInfo, networkSettings) {
	byMAC := make(map[string]networkSettings, len(networks))
	for _, s := range networks {
		byMAC[strings.ToLower(s.MacAddress)] = s
	}

	var primary networkSettings
	var nics []types.GuestNicInfo
	for _, card := range ethernetCards(svm.vm) {
		connected := nicConnected(card)
		nic := types.GuestNicInfo{
			Network:        nicNetworkName(ctx, card),
			MacAddress:     card.MacAddress,
			Connected:      connected,
			DeviceConfigId: card.Key,
		}
		if s, ok := byMAC[strings.ToLower(card.MacAddress)]; ok && connected && s.IPAddress != "" {
			if primary.IPAddress == "" {
				primary = s
			}
			nic.IpAddress = []string{s.IPAddress}
			nic.IpConfig = &types.NetIpConfigInfo{
				IpAddress: []types.NetIpConfigInfoIpAddress{{
					IpAddress:    s.IPAddress,
					PrefixLength: int32(s.IPPrefixLen),
					State:        string(types.NetIpConfigInfoIpAddressStatusPreferred),
				}},
			}
		}
		nics = append(nics, nic)
	}
	return nics, primary
}
