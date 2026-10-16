// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package simulator

import (
	"net"
	"strconv"
	"strings"

	"github.com/vmware/govmomi/toolbox"
	"github.com/vmware/govmomi/vim25/types"
)

// setGuestInfo handles "SetGuestInfo <type> <value>", which vmtoolsd's
// guestInfo plugin sends with two spaces before the type
// (services/plugins/guestInfo/guestInfoServer.c). args is everything after
// "SetGuestInfo ", NUL terminator included: a binary value is not trimmed.
//
// INFO_DNS_NAME sets guest.hostName. INFO_IPADDRESS_V2 and INFO_IPADDRESS_V3
// carrying a NicInfoV3 set guest.net, guest.ipAddress and guest.ipStack.
// Other types, and a nicinfo version other than 3, are accepted and ignored.
func (s *guestRPCServer) setGuestInfo(ctx *Context, args string) string {
	kind, value, ok := strings.Cut(strings.TrimLeft(args, " "), " ")
	if !ok {
		return "0 Bad syntax for SetGuestInfo"
	}
	n, err := strconv.Atoi(kind)
	if err != nil {
		return "0 Bad syntax for SetGuestInfo"
	}

	switch n {
	case toolbox.GuestInfoDNSName:
		name := strings.TrimRight(value, "\x00")
		ctx.AutoUpdate(s.vm, func() {
			s.vm.Guest.HostName = name
			s.vm.Summary.Guest.HostName = name
		})

	case toolbox.GuestInfoIPAddressV2, toolbox.GuestInfoIPAddressV3:
		var info toolbox.GuestNicInfo
		if err := toolbox.DecodeXDR([]byte(value), &info); err != nil {
			return "0 Invalid nicinfo"
		}
		if info.Version != 3 || info.V3 == nil {
			break
		}
		ctx.AutoUpdate(s.vm, func() {
			applyGuestNicInfo(ctx, s.vm, info.V3)
		})
		if s.reported != nil {
			s.reported.Store(true)
		}
	}

	return "1 "
}

// applyGuestNicInfo sets vm's guest network identity from the guest's own
// report, as hostd does from vmtoolsd's nicinfo. guest.net has one entry per
// virtual NIC, in device order, filled from the guest NIC with the same MAC,
// followed by the guest NICs that match no virtual NIC, with DeviceConfigId -1.
// The caller holds the VM lock.
func applyGuestNicInfo(ctx *Context, vm *VirtualMachine, info *toolbox.NicInfoV3) {
	guestNics := make(map[string]int, len(info.Nics)) // MAC to index in info.Nics
	for i, nic := range info.Nics {
		guestNics[strings.ToLower(nic.MacAddress)] = i
	}

	// netIndex maps an index in info.Nics to an index in guest.net, for routes.
	netIndex := make(map[int]int, len(info.Nics))
	var nets []types.GuestNicInfo

	for _, device := range vm.Config.Hardware.Device {
		card, ok := device.(types.BaseVirtualEthernetCard)
		if !ok {
			continue
		}
		c := card.GetVirtualEthernetCard()
		entry := types.GuestNicInfo{
			Network:        guestNicNetwork(ctx, vm, c),
			MacAddress:     c.MacAddress,
			Connected:      c.Connectable == nil || c.Connectable.Connected,
			DeviceConfigId: c.Key,
		}
		if i, ok := guestNics[strings.ToLower(c.MacAddress)]; ok {
			fillGuestNicInfo(&entry, &info.Nics[i])
			netIndex[i] = len(nets)
			delete(guestNics, strings.ToLower(c.MacAddress))
		}
		nets = append(nets, entry)
	}

	for i := range info.Nics {
		if _, ok := guestNics[strings.ToLower(info.Nics[i].MacAddress)]; !ok {
			continue // matched to a virtual NIC
		}
		entry := types.GuestNicInfo{
			MacAddress:     info.Nics[i].MacAddress,
			DeviceConfigId: -1,
		}
		fillGuestNicInfo(&entry, &info.Nics[i])
		netIndex[i] = len(nets)
		nets = append(nets, entry)
	}

	vm.Guest.Net = nets
	vm.Guest.IpAddress = primaryGuestIPv4(nets)
	vm.Summary.Guest.IpAddress = vm.Guest.IpAddress
	vm.Guest.IpStack = guestStackInfo(info, netIndex)
}

// guestNicNetwork returns the name of the network c is backed by, as
// guest.net reports it.
func guestNicNetwork(ctx *Context, vm *VirtualMachine, c *types.VirtualEthernetCard) string {
	for _, n := range vm.Guest.Net {
		if n.DeviceConfigId == c.Key && n.Network != "" {
			return n.Network
		}
	}

	switch b := c.Backing.(type) {
	case *types.VirtualEthernetCardNetworkBackingInfo:
		return b.DeviceName
	case *types.VirtualEthernetCardDistributedVirtualPortBackingInfo:
		ref := types.ManagedObjectReference{Type: "DistributedVirtualPortgroup", Value: b.Port.PortgroupKey}
		if pg, ok := ctx.Map.Get(ref).(*DistributedVirtualPortgroup); ok {
			return pg.Name
		}
	}
	return ""
}

// fillGuestNicInfo copies the guest's addresses and DNS configuration for one
// NIC into entry.
func fillGuestNicInfo(entry *types.GuestNicInfo, nic *toolbox.GuestNicV3) {
	config := &types.NetIpConfigInfo{}
	for _, e := range nic.IPs {
		ip := e.Address.IP()
		if ip == nil {
			continue
		}
		entry.IpAddress = append(entry.IpAddress, ip.String())

		addr := types.NetIpConfigInfoIpAddress{
			IpAddress:    ip.String(),
			PrefixLength: int32(e.PrefixLength),
		}
		if e.Origin != nil {
			addr.Origin = nicInfoOrigin(*e.Origin)
		}
		if e.Status != nil {
			addr.State = nicInfoStatus(*e.Status)
		}
		config.IpAddress = append(config.IpAddress, addr)
	}
	if len(config.IpAddress) != 0 {
		entry.IpConfig = config
	}

	entry.DnsConfig = netDNSConfig(nic.DNSConfigInfo)
}

// nicInfoOrigin maps a nicinfo.x IpAddressOrigin to its vim25 value.
func nicInfoOrigin(origin int32) string {
	switch origin {
	case 2: // IAO_MANUAL
		return string(types.NetIpConfigInfoIpAddressOriginManual)
	case 4: // IAO_DHCP
		return string(types.NetIpConfigInfoIpAddressOriginDhcp)
	case 5: // IAO_LINKLAYER
		return string(types.NetIpConfigInfoIpAddressOriginLinklayer)
	case 6: // IAO_RANDOM
		return string(types.NetIpConfigInfoIpAddressOriginRandom)
	default: // IAO_OTHER
		return string(types.NetIpConfigInfoIpAddressOriginOther)
	}
}

// nicInfoStatus maps a nicinfo.x IpAddressStatus to its vim25 value.
// IAS_OPTIMISTIC has none and is reported as unknown.
func nicInfoStatus(status int32) string {
	switch status {
	case 1: // IAS_PREFERRED
		return string(types.NetIpConfigInfoIpAddressStatusPreferred)
	case 2: // IAS_DEPRECATED
		return string(types.NetIpConfigInfoIpAddressStatusDeprecated)
	case 3: // IAS_INVALID
		return string(types.NetIpConfigInfoIpAddressStatusInvalid)
	case 4: // IAS_INACCESSIBLE
		return string(types.NetIpConfigInfoIpAddressStatusInaccessible)
	case 6: // IAS_TENTATIVE
		return string(types.NetIpConfigInfoIpAddressStatusTentative)
	case 7: // IAS_DUPLICATE
		return string(types.NetIpConfigInfoIpAddressStatusDuplicate)
	default:
		return string(types.NetIpConfigInfoIpAddressStatusUnknown)
	}
}

// primaryGuestIPv4 returns the first IPv4 address that is neither loopback
// nor link-local, from the first entry in nets that has one. nets[0] is the
// VM's first virtual NIC.
func primaryGuestIPv4(nets []types.GuestNicInfo) string {
	for _, n := range nets {
		for _, a := range n.IpAddress {
			ip := net.ParseIP(a)
			if ip.To4() != nil && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() {
				return a
			}
		}
	}
	return ""
}

// guestStackInfo returns the guest's global DNS configuration and routes. A
// route's gateway device is the index in guest.net of the NIC it uses; a
// route on a NIC not in guest.net is dropped.
func guestStackInfo(info *toolbox.NicInfoV3, netIndex map[int]int) []types.GuestStackInfo {
	stack := types.GuestStackInfo{DnsConfig: netDNSConfig(info.DNSConfigInfo)}

	var routes []types.NetIpRouteConfigInfoIpRoute
	for _, r := range info.Routes {
		dest := r.Dest.IP()
		device, ok := netIndex[int(r.IfIndex)]
		if dest == nil || !ok {
			continue
		}
		route := types.NetIpRouteConfigInfoIpRoute{
			Network:      dest.String(),
			PrefixLength: int32(r.PrefixLength),
			Gateway:      types.NetIpRouteConfigInfoGateway{Device: strconv.Itoa(device)},
		}
		if r.NextHop != nil {
			if hop := r.NextHop.IP(); hop != nil {
				route.Gateway.IpAddress = hop.String()
			}
		}
		routes = append(routes, route)
	}
	if len(routes) != 0 {
		stack.IpRouteConfig = &types.NetIpRouteConfigInfo{IpRoute: routes}
	}

	if stack.DnsConfig == nil && stack.IpRouteConfig == nil {
		return nil
	}
	return []types.GuestStackInfo{stack}
}

// netDNSConfig maps a nicinfo.x DnsConfigInfo to its vim25 value, or nil.
func netDNSConfig(dns *toolbox.DNSConfigInfo) *types.NetDnsConfigInfo {
	if dns == nil {
		return nil
	}

	config := &types.NetDnsConfigInfo{SearchDomain: dns.Search}
	if dns.HostName != nil {
		config.HostName = *dns.HostName
	}
	if dns.DomainName != nil {
		config.DomainName = *dns.DomainName
	}
	for _, server := range dns.Servers {
		if ip := server.IP(); ip != nil {
			config.IpAddress = append(config.IpAddress, ip.String())
		}
	}
	return config
}
