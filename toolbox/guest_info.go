// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package toolbox

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"math/bits"
	"net"
	"os"
	"path"
	"strconv"
	"strings"

	xdr "github.com/rasky/go-xdr/xdr2"
)

// GuestInfoType values from open-vm-tools lib/include/guestInfo.h, the type
// field of a "SetGuestInfo" RPCI command.
const (
	GuestInfoDNSName     = 1  // INFO_DNS_NAME: the guest's node name, as text
	GuestInfoIPAddressV2 = 9  // INFO_IPADDRESS_V2: XDR GuestNicInfo, at most 64 IPs per NIC
	GuestInfoIPAddressV3 = 10 // INFO_IPADDRESS_V3: XDR GuestNicInfo
)

// Defs from: open-vm-tools/lib/guestRpc/nicinfo.x

type TypedIPAddress struct {
	Type    int32
	Address []byte
}

type IPAddressEntry struct {
	Address      TypedIPAddress
	PrefixLength uint32
	Origin       *int32 `xdr:"optional"`
	Status       *int32 `xdr:"optional"`
}

type InetCidrRouteEntry struct {
	Dest         TypedIPAddress
	PrefixLength uint32
	NextHop      *TypedIPAddress `xdr:"optional"`
	IfIndex      uint32
	Type         int32
	Metric       uint32
}

type DNSConfigInfo struct {
	HostName   *string `xdr:"optional"`
	DomainName *string `xdr:"optional"`
	Servers    []TypedIPAddress
	Search     []string
}

type WinsConfigInfo struct {
	Primary   TypedIPAddress
	Secondary TypedIPAddress
}

type DhcpConfigInfo struct {
	Enabled  bool
	Settings string
}

type GuestNicV3 struct {
	MacAddress       string
	IPs              []IPAddressEntry
	DNSConfigInfo    *DNSConfigInfo  `xdr:"optional"`
	WinsConfigInfo   *WinsConfigInfo `xdr:"optional"`
	DhcpConfigInfov4 *DhcpConfigInfo `xdr:"optional"`
	DhcpConfigInfov6 *DhcpConfigInfo `xdr:"optional"`
}

type NicInfoV3 struct {
	Nics             []GuestNicV3
	Routes           []InetCidrRouteEntry
	DNSConfigInfo    *DNSConfigInfo  `xdr:"optional"`
	WinsConfigInfo   *WinsConfigInfo `xdr:"optional"`
	DhcpConfigInfov4 *DhcpConfigInfo `xdr:"optional"`
	DhcpConfigInfov6 *DhcpConfigInfo `xdr:"optional"`
}

type GuestNicInfo struct {
	Version int32
	V3      *NicInfoV3 `xdr:"optional"`
}

func EncodeXDR(val any) ([]byte, error) {
	var buf bytes.Buffer

	_, err := xdr.Marshal(&buf, val)
	if err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

func DecodeXDR(buf []byte, val any) error {
	r := bytes.NewReader(buf)
	_, err := xdr.Unmarshal(r, val)
	return err
}

func NewGuestNicInfo() *GuestNicInfo {
	return &GuestNicInfo{
		Version: 3,
		V3:      &NicInfoV3{},
	}
}

func (nic *GuestNicV3) AddIP(addr net.Addr) {
	ip, ok := addr.(*net.IPNet)
	if !ok {
		return
	}

	size, _ := ip.Mask.Size()

	// nicinfo.x defines enum IpAddressStatus, but vmtoolsd only uses IAS_PREFERRED
	var status int32 = 1 // IAS_PREFERRED

	e := IPAddressEntry{
		Address:      NewTypedIPAddress(ip.IP),
		PrefixLength: uint32(size),
		Status:       &status,
	}

	nic.IPs = append(nic.IPs, e)
}

// NewTypedIPAddress returns ip as a nicinfo.x TypedIpAddress: IAT_IPV4 with 4
// bytes, or IAT_IPV6 with 16.
func NewTypedIPAddress(ip net.IP) TypedIPAddress {
	if ip4 := ip.To4(); ip4 != nil {
		return TypedIPAddress{Type: 1 /* IAT_IPV4 */, Address: []byte(ip4)}
	}
	return TypedIPAddress{Type: 2 /* IAT_IPV6 */, Address: []byte(ip.To16())}
}

// IP returns the address as a net.IP, or nil if it is not an IPv4 or IPv6
// address.
func (a TypedIPAddress) IP() net.IP {
	switch {
	case a.Type == 1 && len(a.Address) == net.IPv4len:
		return net.IP(a.Address)
	case a.Type == 2 && len(a.Address) == net.IPv6len:
		return net.IP(a.Address)
	}
	return nil
}

// GuestInfoCommand returns the binary "SetGuestInfo" request vmtoolsd sends
// for kind: the command, two spaces, the type, a space, then req.
func GuestInfoCommand(kind int, req []byte) []byte {
	request := fmt.Sprintf("SetGuestInfo  %d ", kind)
	return append([]byte(request), req...)
}

var (
	netInterfaces = net.Interfaces
	maxNics       = 16 // guestRpc/nicinfo.x:NICINFO_MAX_NICS

	hostname     = os.Hostname
	resolvConf   = "/etc/resolv.conf"
	procNetRoute = "/proc/net/route"

	// excludeNics is vmtoolsd's default exclude-nics setting on Linux
	// (guestInfoServer.c GUESTINFO_DEFAULT_IFACE_EXCLUDES): container and
	// overlay interfaces that say nothing about the VM's own network identity.
	excludeNics = []string{"docker*", "veth*", "virbr*", "antrea-*", "cali*"}
)

const (
	maxRoutes      = 100 // guestRpc/nicinfo.x:NICINFO_MAX_ROUTES
	maxDNSServers  = 16  // guestRpc/nicinfo.x:DNSINFO_MAX_SERVERS
	maxDNSSuffixes = 10  // guestRpc/nicinfo.x:DNSINFO_MAX_SUFFIXES
)

// isExcludedNic reports whether name matches an excludeNics pattern, as
// lib/nicInfo/nicInfo.c GuestInfo_IfaceIsExcluded does.
func isExcludedNic(name string) bool {
	for _, pattern := range excludeNics {
		if ok, _ := path.Match(pattern, name); ok {
			return true
		}
	}
	return false
}

func DefaultGuestNicInfo() *GuestNicInfo {
	proto := NewGuestNicInfo()
	info := proto.V3
	// #nosec: Errors unhandled
	ifs, _ := netInterfaces()

	index := make(map[string]uint32) // interface name to index in info.Nics

	for _, i := range ifs {
		if i.Flags&net.FlagUp == 0 || i.Flags&net.FlagLoopback != 0 || isExcludedNic(i.Name) {
			continue
		}

		if len(i.HardwareAddr) == 0 {
			continue // Not useful from outside the guest without a MAC
		}

		// #nosec: Errors unhandled
		addrs, _ := i.Addrs()

		if len(addrs) == 0 {
			continue // Not useful from outside the guest without an IP
		}

		nic := GuestNicV3{
			MacAddress: i.HardwareAddr.String(),
		}

		for _, addr := range addrs {
			nic.AddIP(addr)
		}

		index[i.Name] = uint32(len(info.Nics))
		info.Nics = append(info.Nics, nic)

		if len(info.Nics) >= maxNics {
			break
		}
	}

	info.Routes = guestRoutes(index)
	info.DNSConfigInfo = guestDNSConfig()

	return proto
}

// guestRoutes returns the IPv4 routes that are up and use an interface in
// index, read from /proc/net/route as lib/nicInfo/nicInfoPosix.c
// RecordRoutingInfoIPv4 does. Each route's IfIndex is the interface's index
// in NicInfoV3.Nics. It returns nil where /proc/net/route does not exist.
// IPv6 routes are not reported.
func guestRoutes(index map[string]uint32) []InetCidrRouteEntry {
	const (
		rtfUp      = 0x1 // RTF_UP
		rtfGateway = 0x2 // RTF_GATEWAY
	)

	f, err := os.Open(procNetRoute)
	if err != nil {
		return nil
	}
	defer f.Close()

	var routes []InetCidrRouteEntry
	scanner := bufio.NewScanner(f)
	scanner.Scan() // header
	for scanner.Scan() && len(routes) < maxRoutes {
		// Iface Destination Gateway Flags RefCnt Use Metric Mask ...
		fields := strings.Fields(scanner.Text())
		if len(fields) < 8 {
			continue
		}
		ifIndex, ok := index[fields[0]]
		if !ok {
			continue
		}
		flags, err := strconv.ParseUint(fields[3], 16, 32)
		if err != nil || flags&rtfUp == 0 {
			continue
		}
		dest, err1 := procNetIPv4(fields[1])
		gateway, err2 := procNetIPv4(fields[2])
		mask, err3 := procNetIPv4(fields[7])
		metric, err4 := strconv.ParseUint(fields[6], 10, 32)
		if err1 != nil || err2 != nil || err3 != nil || err4 != nil {
			continue
		}

		route := InetCidrRouteEntry{
			Dest:         NewTypedIPAddress(dest),
			PrefixLength: uint32(bits.OnesCount32(binary.BigEndian.Uint32(mask))),
			IfIndex:      ifIndex,
			Metric:       uint32(metric),
		}
		if flags&rtfGateway != 0 {
			hop := NewTypedIPAddress(gateway)
			route.NextHop = &hop
		}
		routes = append(routes, route)
	}

	return routes
}

// procNetIPv4 parses an address from /proc/net/route, which prints the
// network-order value as a host-order hex integer.
func procNetIPv4(s string) (net.IP, error) {
	v, err := strconv.ParseUint(s, 16, 32)
	if err != nil {
		return nil, err
	}
	ip := make(net.IP, net.IPv4len)
	binary.NativeEndian.PutUint32(ip, uint32(v))
	return ip, nil
}

// guestDNSConfig returns the node name and resolver configuration, as
// lib/nicInfo/nicInfoPosix.c RecordResolverInfo does, or nil if the node name
// is unavailable. The resolver configuration is read from /etc/resolv.conf: as
// resolv.conf(5) specifies, the last "domain" or "search" line wins, and the
// domain is the first search suffix.
func guestDNSConfig() *DNSConfigInfo {
	name, err := hostname()
	if err != nil || name == "" {
		return nil
	}
	dns := &DNSConfigInfo{HostName: &name}

	f, err := os.Open(resolvConf)
	if err != nil {
		return dns
	}
	defer f.Close()

	var search []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 {
			continue
		}
		switch fields[0] {
		case "nameserver":
			if ip := net.ParseIP(fields[1]); ip != nil && len(dns.Servers) < maxDNSServers {
				dns.Servers = append(dns.Servers, NewTypedIPAddress(ip))
			}
		case "domain":
			search = fields[1:2]
		case "search":
			search = fields[1:]
		}
	}

	if len(search) > maxDNSSuffixes {
		search = search[:maxDNSSuffixes]
	}
	if len(search) != 0 {
		dns.DomainName = &search[0]
		dns.Search = search
	}

	return dns
}

func GuestInfoNicInfoRequest() ([]byte, error) {
	r, err := EncodeXDR(DefaultGuestNicInfo())
	if err != nil {
		return nil, err
	}

	// INFO_IPADDRESS_V2 carrying a NicInfoV3, the second of vmtoolsd's
	// NicInfoMethod fallbacks (guestInfoServer.c NIC_INFO_V3_WITH_INFO_IPADDRESS_V2).
	return GuestInfoCommand(GuestInfoIPAddressV2, r), nil
}
