// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package simulator

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
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
// default network, as before. With one, the first connected vNIC on a domain joins the domain's network, and each
// further vNIC on that domain is a macvlan sub-interface of the first one's interface, because podman joins a
// container to a network once.
func (svm *simVM) containerNICs(ctx *Context) ([]containerNIC, error) {
	if ctx.Map.underlay == nil {
		return []containerNIC{{MAC: firstNICMacAddress(svm.vm)}}, nil
	}

	var nics []containerNIC
	parents := make(map[string]string)
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
		name, err := ctx.Map.underlay.Network(d)
		if err != nil {
			return nil, err
		}
		nic := containerNIC{Network: name, MAC: card.MacAddress, Name: fmt.Sprintf("eth%d", i)}
		if parent, ok := parents[d.Key()]; ok {
			nic.Parent = parent
		} else {
			parents[d.Key()] = nic.Name
		}
		nics = append(nics, nic)
	}
	return nics, nil
}

// subNICs returns the vNICs the container would have as sub-interfaces at power-on, without changing any vNIC's
// connected state. A container restarted in place lost them with its network namespace.
func (svm *simVM) subNICs(ctx *Context) ([]containerNIC, error) {
	if ctx.Map.underlay == nil {
		return nil, nil
	}
	var nics []containerNIC
	parents := make(map[string]string)
	for i, card := range ethernetCards(svm.vm) {
		if !nicConnected(card) {
			continue
		}
		d, err := svm.domainOf(ctx, card)
		if err != nil {
			return nil, err
		}
		network, err := ctx.Map.underlay.Network(d)
		if err != nil {
			return nil, err
		}
		name := fmt.Sprintf("eth%d", i)
		if parent, ok := parents[d.Key()]; ok {
			nics = append(nics, containerNIC{Network: network, MAC: card.MacAddress, Name: name, Parent: parent})
		} else {
			parents[d.Key()] = name
		}
	}
	return nics, nil
}

// netLink is an interface in a container's network namespace.
type netLink struct {
	name   string
	parent string
	mac    string
}

// parseLinks parses `ip -o link` output.
func parseLinks(out string) []netLink {
	var links []netLink
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		name, parent, _ := strings.Cut(strings.TrimSuffix(f[1], ":"), "@")
		l := netLink{name: name, parent: parent}
		for i := range f[:len(f)-1] {
			if f[i] == "link/ether" {
				l.mac = strings.ToLower(f[i+1])
			}
		}
		links = append(links, l)
	}
	return links
}

// links returns the interfaces in the container's network namespace, and the namespace owner's PID.
func (svm *simVM) links() ([]netLink, int, error) {
	out, err := podmanOutput("inspect", "--format", "{{.State.Pid}}", svm.c.id)
	if err != nil {
		return nil, 0, err
	}
	pid, err := strconv.Atoi(out)
	if err != nil || pid == 0 {
		return nil, 0, fmt.Errorf("%s: container %s has no PID %q", svm.vm.Name, svm.c.id, out)
	}
	out, err = nsenter(pid, "", "ip", "-o", "link")
	if err != nil {
		return nil, 0, err
	}
	return parseLinks(out), pid, nil
}

// linkByMAC returns the interface with mac.
func linkByMAC(links []netLink, mac string) (netLink, bool) {
	for _, l := range links {
		if l.mac != "" && strings.EqualFold(l.mac, mac) {
			return l, true
		}
	}
	return netLink{}, false
}

// addSubNICs creates the sub-interfaces among nics, each on its parent with its MAC. One whose MAC is already
// present, as after an unpause, is left as is.
func (svm *simVM) addSubNICs(nics []containerNIC) error {
	var links []netLink
	pid := 0
	for _, nic := range nics {
		if nic.Parent == "" {
			continue
		}
		if pid == 0 {
			var err error
			if links, pid, err = svm.links(); err != nil {
				return err
			}
		}
		if _, ok := linkByMAC(links, nic.MAC); ok {
			continue
		}
		// A container restarted in place may have rejoined under another name, so the parent is found by MAC.
		if mac, joined, err := svm.joinedMAC(nic.Network); err != nil {
			return err
		} else if l, ok := linkByMAC(links, mac); joined && ok {
			nic.Parent = l.name
		}
		if err := addMacvlan(pid, nic); err != nil {
			return err
		}
	}
	return nil
}

// addMacvlan creates nic as a macvlan sub-interface of its parent, in bridge mode so that it reaches the domain
// like a joined interface, and brings it up as podman does the interfaces it joins. The macvlan is in the
// container's namespace only, so it goes with the container.
func addMacvlan(pid int, nic containerNIC) error {
	args := []string{"ip", "link", "add", "link", nic.Parent, "name", nic.Name}
	if nic.MAC != "" {
		args = append(args, "address", nic.MAC)
	}
	args = append(args, "type", "macvlan", "mode", "bridge")
	if _, err := nsenter(pid, "", args...); err != nil {
		return err
	}
	_, err := nsenter(pid, "", "ip", "link", "set", "dev", nic.Name, "up")
	return err
}

// joinedMAC returns the MAC of the container's interface on network, if the container has joined it.
func (svm *simVM) joinedMAC(network string) (string, bool, error) {
	out, err := podmanOutput("inspect", "--format", "{{json .NetworkSettings.Networks}}", svm.c.id)
	if err != nil {
		return "", false, err
	}
	var networks map[string]struct{ MacAddress string }
	if err := json.Unmarshal([]byte(out), &networks); err != nil {
		return "", false, fmt.Errorf("%s: networks of container %s: %w", svm.vm.Name, svm.c.id, err)
	}
	n, ok := networks[network]
	return n.MacAddress, ok, nil
}

// applyNIC moves a running container's interface from its before state to its after state: leave the old
// network, join the new one with the same MAC. A vNIC joins a network the container has not joined with podman,
// which names the new interface itself, so the in-guest name can change on a move; guests find the interface by
// MAC. A vNIC on a network the container has already joined is a macvlan sub-interface of that interface instead.
func (svm *simVM) applyNIC(before, after nicState) error {
	moved := before.network != after.network || before.mac != after.mac
	if before.attached && (!after.attached || moved) {
		if err := svm.leave(before); err != nil {
			return err
		}
	}
	if after.attached && (!before.attached || moved) {
		return svm.join(after)
	}
	return nil
}

// join gives the container an interface for st.
func (svm *simVM) join(st nicState) error {
	mac, joined, err := svm.joinedMAC(st.network)
	if err != nil {
		return err
	}
	if !joined {
		return svm.connect(st.network, st.mac)
	}

	links, pid, err := svm.links()
	if err != nil {
		return err
	}
	parent, ok := linkByMAC(links, mac)
	if !ok {
		return fmt.Errorf("%s: no interface with MAC %s on %s", svm.vm.Name, mac, st.network)
	}
	if _, ok := linkByMAC(links, st.mac); ok {
		return nil
	}
	return addMacvlan(pid, containerNIC{MAC: st.mac, Name: freeLinkName(links), Parent: parent.name})
}

// leave removes the container's interface for st. A sub-interface is deleted. The joined interface is
// disconnected, which deletes its sub-interfaces with it, so the first of them rejoins the network with its MAC
// and the others are recreated on it.
func (svm *simVM) leave(st nicState) error {
	mac, joined, err := svm.joinedMAC(st.network)
	if err != nil {
		return err
	}
	links, pid, err := svm.links()
	if err != nil {
		return err
	}
	if !joined || !strings.EqualFold(mac, st.mac) {
		l, ok := linkByMAC(links, st.mac)
		if !ok {
			return nil
		}
		_, err := nsenter(pid, "", "ip", "link", "del", "dev", l.name)
		return err
	}

	var subs []netLink
	if parent, ok := linkByMAC(links, mac); ok {
		for _, l := range links {
			if l.parent == parent.name {
				subs = append(subs, l)
			}
		}
	}
	if _, err := podmanOutput("network", "disconnect", st.network, svm.c.id); err != nil {
		return err
	}
	if len(subs) == 0 {
		return nil
	}
	if err := svm.connect(st.network, subs[0].mac); err != nil {
		return err
	}
	if len(subs) == 1 {
		return nil
	}
	links, pid, err = svm.links()
	if err != nil {
		return err
	}
	parent, ok := linkByMAC(links, subs[0].mac)
	if !ok {
		return fmt.Errorf("%s: no interface with MAC %s on %s", svm.vm.Name, subs[0].mac, st.network)
	}
	for _, l := range subs[1:] {
		if err := addMacvlan(pid, containerNIC{MAC: l.mac, Name: l.name, Parent: parent.name}); err != nil {
			return err
		}
	}
	return nil
}

// connect joins the container to network with mac.
func (svm *simVM) connect(network, mac string) error {
	args := []string{"network", "connect"}
	if mac != "" {
		args = append(args, "--mac-address", mac)
	}
	args = append(args, network, svm.c.id)
	_, err := podmanOutput(args...)
	return err
}

// freeLinkName returns the first eth<n> not in links.
func freeLinkName(links []netLink) string {
	used := make(map[string]bool, len(links))
	for _, l := range links {
		used[l.name] = true
	}
	for n := 0; ; n++ {
		if name := fmt.Sprintf("eth%d", n); !used[name] {
			return name
		}
	}
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
