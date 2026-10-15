// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package toolbox

import (
	"bytes"
	"encoding/binary"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// recordingChannelOut records each RPCI request and replies with reply.
type recordingChannelOut struct {
	mu       sync.Mutex
	requests [][]byte
	reply    []byte
	sent     chan struct{}
}

func (c *recordingChannelOut) Start() error { return nil }
func (c *recordingChannelOut) Stop() error  { return nil }

func (c *recordingChannelOut) Send(buf []byte) error {
	c.mu.Lock()
	c.requests = append(c.requests, bytes.Clone(buf))
	c.mu.Unlock()
	if c.sent != nil {
		c.sent <- struct{}{}
	}
	return nil
}

func (c *recordingChannelOut) Receive() ([]byte, error) {
	return c.reply, nil
}

// take returns the requests recorded so far and forgets them.
func (c *recordingChannelOut) take() [][]byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	r := c.requests
	c.requests = nil
	return r
}

func setGuestInfoKind(t *testing.T, req []byte) int {
	t.Helper()
	rest, ok := bytes.CutPrefix(req, []byte("SetGuestInfo  "))
	if !ok {
		t.Fatalf("request %q is not SetGuestInfo", req)
	}
	kind, _, _ := bytes.Cut(rest, []byte(" "))
	n, err := strconv.Atoi(string(kind))
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// testHostname is the node name the tests' hostname returns. The Service
// reads it from its own goroutine.
type testHostname struct{ atomic.Value }

func (h *testHostname) set(name string) { h.Store(name) }

func setHostname(t *testing.T, name string) *testHostname {
	t.Helper()
	h := new(testHostname)
	h.set(name)
	saved := hostname
	hostname = func() (string, error) { return h.Load().(string), nil }
	t.Cleanup(func() { hostname = saved })
	return h
}

func TestGatherGuestInfo(t *testing.T) {
	name := setHostname(t, "guest-a")

	out := &recordingChannelOut{reply: rpciOK}
	s := NewService(NewNoopChannelIn(), out)
	s.out = &ChannelOut{out} // no trace

	kinds := func() []int {
		var k []int
		for _, r := range out.take() {
			k = append(k, setGuestInfoKind(t, r))
		}
		return k
	}

	s.gatherGuestInfo()
	reqs := out.take()
	if len(reqs) != 2 {
		t.Fatalf("first gather sent %d requests, want 2", len(reqs))
	}
	if want := "SetGuestInfo  1 guest-a\x00"; string(reqs[0]) != want {
		t.Errorf("DNS name request %q, want %q", reqs[0], want)
	}
	nic, ok := bytes.CutPrefix(reqs[1], []byte("SetGuestInfo  9 "))
	if !ok {
		t.Fatalf("nicinfo request %q", reqs[1])
	}
	var info GuestNicInfo
	if err := DecodeXDR(nic, &info); err != nil {
		t.Fatal(err)
	}
	if info.Version != 3 || info.V3 == nil || info.V3.DNSConfigInfo == nil || *info.V3.DNSConfigInfo.HostName != "guest-a" {
		t.Errorf("nicinfo %+v does not carry the DNS name", info)
	}

	s.gatherGuestInfo()
	if k := kinds(); len(k) != 0 {
		t.Errorf("unchanged gather sent %v", k)
	}

	// The node name is in both payloads.
	name.set("guest-b")
	s.gatherGuestInfo()
	if k := kinds(); !reflect.DeepEqual(k, []int{GuestInfoDNSName, GuestInfoIPAddressV2}) {
		t.Errorf("gather after rename sent %v", k)
	}

	// A reset clears the cache; Reset itself sends the NIC info.
	if _, err := s.Reset(nil); err != nil {
		t.Fatal(err)
	}
	if k := kinds(); !reflect.DeepEqual(k, []int{GuestInfoIPAddressV2}) {
		t.Errorf("reset sent %v", k)
	}
	s.gatherGuestInfo()
	if k := kinds(); !reflect.DeepEqual(k, []int{GuestInfoDNSName, GuestInfoIPAddressV2}) {
		t.Errorf("gather after reset sent %v", k)
	}

	// A value the VMX refuses is not cached, so it is retried.
	name.set("guest-c")
	out.reply = rpciERR
	s.gatherGuestInfo()
	_ = out.take()
	out.reply = rpciOK
	s.gatherGuestInfo()
	if k := kinds(); !reflect.DeepEqual(k, []int{GuestInfoDNSName, GuestInfoIPAddressV2}) {
		t.Errorf("gather after refusal sent %v", k)
	}
}

func TestServiceGuestInfoInterval(t *testing.T) {
	name := setHostname(t, "guest")

	out := &recordingChannelOut{reply: rpciOK, sent: make(chan struct{}, 16)}
	s := NewService(new(mockChannelIn), out)
	s.GuestInfoInterval = 10 * time.Millisecond

	wait := func(what string) {
		t.Helper()
		for range 2 {
			select {
			case <-out.sent:
			case <-time.After(5 * time.Second):
				t.Fatalf("no guest info sent %s", what)
			}
		}
	}

	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	wait("on start")
	name.set("renamed")
	wait("on the interval")
	s.Stop()
	s.Wait()

	for _, r := range out.take() {
		if k := setGuestInfoKind(t, r); k != GuestInfoDNSName && k != GuestInfoIPAddressV2 {
			t.Errorf("unexpected request %q", r)
		}
	}
}

func TestServiceGuestInfoIntervalZero(t *testing.T) {
	out := &recordingChannelOut{reply: rpciOK}
	s := NewService(new(mockChannelIn), out)

	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	s.Stop()
	s.Wait()

	if r := out.take(); len(r) != 0 {
		t.Errorf("sent %q with GuestInfoInterval 0", r)
	}
}

// TestServiceStartReports checks what a Service sends unprompted when it
// starts: its version, if ReportVersion is set, and, on a transport without
// TCLO, its capabilities.
func TestServiceStartReports(t *testing.T) {
	version := []string{
		"info-set guestinfo.vmtools.description " + ToolsDescription + " ",
		"info-set guestinfo.vmtools.versionString ",
		"info-set guestinfo.vmtools.versionNumber 2147483647",
		"info-set guestinfo.vmtools.buildNumber 0",
	}

	tests := []struct {
		name          string
		in            Channel
		reportVersion bool
		want          []string
	}{
		{"TCLO", new(mockChannelIn), false, nil},
		{"TCLO with version", new(mockChannelIn), true, version},
		{"no TCLO", NewNoopChannelIn(), false, capabilities},
		{"no TCLO with version", NewNoopChannelIn(), true, append(append([]string(nil), version...), capabilities...)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := &recordingChannelOut{reply: rpciOK, sent: make(chan struct{}, 16)}
			s := NewService(tt.in, out)
			s.ReportVersion = tt.reportVersion

			if err := s.Start(); err != nil {
				t.Fatal(err)
			}
			for range tt.want {
				select {
				case <-out.sent:
				case <-time.After(5 * time.Second):
					t.Fatal("timeout")
				}
			}
			time.Sleep(20 * time.Millisecond)
			s.Stop()
			s.Wait()

			got := out.take()
			if len(got) != len(tt.want) {
				t.Fatalf("sent %q, want %q", got, tt.want)
			}
			for i, w := range tt.want {
				if !strings.HasPrefix(string(got[i]), w) {
					t.Errorf("request %d: %q, want prefix %q", i, got[i], w)
				}
			}
		})
	}
}

func TestGuestRoutes(t *testing.T) {
	dir := t.TempDir()
	saved := procNetRoute
	procNetRoute = filepath.Join(dir, "route")
	t.Cleanup(func() { procNetRoute = saved })

	ip := func(s string) string {
		var b [4]byte
		copy(b[:], net.ParseIP(s).To4())
		return strings.ToUpper(strconv.FormatUint(uint64(nativeUint32(b)), 16))
	}
	table := "Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\t\tMTU\tWindow\tIRTT\n" +
		"eth0\t" + ip("0.0.0.0") + "\t" + ip("10.0.2.1") + "\t0003\t0\t0\t100\t" + ip("0.0.0.0") + "\t0\t0\t0\n" +
		"eth0\t" + ip("10.0.2.0") + "\t" + ip("0.0.0.0") + "\t0001\t0\t0\t0\t" + ip("255.255.255.0") + "\t0\t0\t0\n" +
		"eth0\t" + ip("10.9.0.0") + "\t" + ip("0.0.0.0") + "\t0000\t0\t0\t0\t" + ip("255.255.0.0") + "\t0\t0\t0\n" +
		"antrea-gw0\t" + ip("192.168.0.0") + "\t" + ip("0.0.0.0") + "\t0001\t0\t0\t0\t" + ip("255.255.255.0") + "\t0\t0\t0\n"
	if err := os.WriteFile(procNetRoute, []byte(table), 0o600); err != nil {
		t.Fatal(err)
	}

	routes := guestRoutes(map[string]uint32{"eth0": 0})
	if len(routes) != 2 {
		t.Fatalf("routes=%+v, want the 2 up routes on eth0", routes)
	}
	def := routes[0]
	if !def.Dest.IP().Equal(net.IPv4zero) || def.PrefixLength != 0 || def.NextHop == nil ||
		!def.NextHop.IP().Equal(net.ParseIP("10.0.2.1")) || def.Metric != 100 || def.IfIndex != 0 {
		t.Errorf("default route %+v", def)
	}
	link := routes[1]
	if !link.Dest.IP().Equal(net.ParseIP("10.0.2.0")) || link.PrefixLength != 24 || link.NextHop != nil {
		t.Errorf("link route %+v", link)
	}

	procNetRoute = filepath.Join(dir, "missing")
	if r := guestRoutes(map[string]uint32{"eth0": 0}); r != nil {
		t.Errorf("routes without /proc/net/route: %+v", r)
	}
}

func TestGuestDNSConfig(t *testing.T) {
	setHostname(t, "node-1")

	dir := t.TempDir()
	saved := resolvConf
	resolvConf = filepath.Join(dir, "resolv.conf")
	t.Cleanup(func() { resolvConf = saved })

	conf := "# comment\nnameserver 10.0.2.3\nnameserver fd00::3\nnameserver bogus\n" +
		"domain ignored.example\nsearch corp.example example\noptions ndots:5\n"
	if err := os.WriteFile(resolvConf, []byte(conf), 0o600); err != nil {
		t.Fatal(err)
	}

	dns := guestDNSConfig()
	if dns == nil || *dns.HostName != "node-1" {
		t.Fatalf("dns=%+v", dns)
	}
	if dns.DomainName == nil || *dns.DomainName != "corp.example" {
		t.Errorf("domain=%v", dns.DomainName)
	}
	if !reflect.DeepEqual(dns.Search, []string{"corp.example", "example"}) {
		t.Errorf("search=%v", dns.Search)
	}
	if len(dns.Servers) != 2 || dns.Servers[0].Type != 1 || dns.Servers[1].Type != 2 ||
		!dns.Servers[1].IP().Equal(net.ParseIP("fd00::3")) {
		t.Errorf("servers=%+v", dns.Servers)
	}

	if _, err := EncodeXDR(dns); err != nil {
		t.Fatal(err)
	}
}

func TestExcludedNic(t *testing.T) {
	for name, want := range map[string]bool{
		"eth0": false, "ens192": false, "docker0": true, "veth1234": true,
		"virbr0": true, "antrea-gw0": true, "cali123": true,
	} {
		if got := isExcludedNic(name); got != want {
			t.Errorf("isExcludedNic(%q)=%t", name, got)
		}
	}
}

func nativeUint32(b [4]byte) uint32 {
	return binary.NativeEndian.Uint32(b[:])
}

func TestGuestRPCPing(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteGuestRPCPing(&buf); err != nil {
		t.Fatal(err)
	}
	if err := WriteDataMapPacket(&buf, []byte("ping"), true); err != nil {
		t.Fatal(err)
	}

	p, err := ReadGuestRPCPacket(&buf)
	if err != nil || p.Type != GuestRPCPacketPing || p.Payload != nil || p.FastClose {
		t.Errorf("ping read as %+v, %v", p, err)
	}
	p, err = ReadGuestRPCPacket(&buf)
	if err != nil || p.Type != GuestRPCPacketData || string(p.Payload) != "ping" || !p.FastClose {
		t.Errorf("data read as %+v, %v", p, err)
	}
}

// fakeGuestNIC replaces the guest's NICs with eth0, whose addresses are
// addrs(n) on the n-th query. It returns the number of queries so far.
func fakeGuestNIC(t *testing.T, addrs func(n int32) []net.Addr) *atomic.Int32 {
	t.Helper()
	var queries atomic.Int32
	savedIfs, savedAddrs := netInterfaces, ifaceAddrs
	netInterfaces = func() ([]net.Interface, error) {
		mac := net.HardwareAddr{0x00, 0x50, 0x56, 0x00, 0x00, 0x01}
		return []net.Interface{{Name: "eth0", Flags: net.FlagUp, HardwareAddr: mac}}, nil
	}
	ifaceAddrs = func(net.Interface) ([]net.Addr, error) {
		return addrs(queries.Add(1)), nil
	}
	t.Cleanup(func() { netInterfaces, ifaceAddrs = savedIfs, savedAddrs })
	return &queries
}

// nicReportAddrs returns the addresses carried by each NIC report in reqs,
// in order, with the requests that are not NIC reports skipped.
func nicReportAddrs(t *testing.T, reqs [][]byte) [][]string {
	t.Helper()
	prefix := string(GuestInfoCommand(GuestInfoIPAddressV2, nil))
	var reports [][]string
	for _, r := range reqs {
		payload, ok := bytes.CutPrefix(r, []byte(prefix))
		if !ok {
			continue
		}
		var info GuestNicInfo
		if err := DecodeXDR(payload, &info); err != nil {
			t.Fatal(err)
		}
		var addrs []string
		for _, nic := range info.V3.Nics {
			for _, e := range nic.IPs {
				addrs = append(addrs, e.Address.IP().String())
			}
		}
		reports = append(reports, addrs)
	}
	return reports
}

func TestServiceGuestInfoRetriesUntilIPv4(t *testing.T) {
	setHostname(t, "guest")
	linkLocal := &net.IPNet{IP: net.ParseIP("fe80::250:56ff:fe00:1"), Mask: net.CIDRMask(64, 128)}
	dhcp := &net.IPNet{IP: net.ParseIP("10.0.2.101"), Mask: net.CIDRMask(24, 32)}

	// As at boot, eth0 has only its IPv6 link-local address for the first
	// gather, and its IPv4 address appears after that.
	queries := fakeGuestNIC(t, func(n int32) []net.Addr {
		if n == 1 {
			return []net.Addr{linkLocal}
		}
		return []net.Addr{linkLocal, dhcp}
	})
	savedInterval := guestInfoRetryInterval
	guestInfoRetryInterval = 10 * time.Millisecond
	t.Cleanup(func() { guestInfoRetryInterval = savedInterval })

	out := &recordingChannelOut{reply: rpciOK, sent: make(chan struct{}, 16)}
	s := NewService(new(mockChannelIn), out)
	// The interval is long enough that only the retry can send the IPv4 report.
	s.GuestInfoInterval = time.Hour

	if err := s.Start(); err != nil {
		t.Fatal(err)
	}

	var reqs [][]byte
	timeout := time.After(5 * time.Second)
	for !slices.ContainsFunc(nicReportAddrs(t, reqs), func(a []string) bool { return slices.Contains(a, "10.0.2.101") }) {
		select {
		case <-out.sent:
			reqs = append(reqs, out.take()...)
		case <-timeout:
			t.Fatalf("no NIC report with an IPv4 address sent; requests %q", reqs)
		}
	}

	// Once the VMX has accepted the IPv4 report, gathering stops.
	stopped := queries.Load()
	time.Sleep(100 * time.Millisecond)
	if n := queries.Load(); n != stopped {
		t.Errorf("gathers continued after the IPv4 report: %d queries, then %d", stopped, n)
	}

	s.Stop()
	s.Wait()
	reqs = append(reqs, out.take()...)

	reports := nicReportAddrs(t, reqs)
	if want := [][]string{{"fe80::250:56ff:fe00:1"}, {"fe80::250:56ff:fe00:1", "10.0.2.101"}}; !reflect.DeepEqual(reports, want) {
		t.Errorf("NIC reports %v, want %v", reports, want)
	}
}

func TestServiceGuestInfoRetryWindow(t *testing.T) {
	setHostname(t, "guest")
	linkLocal := &net.IPNet{IP: net.ParseIP("fe80::250:56ff:fe00:1"), Mask: net.CIDRMask(64, 128)}
	queries := fakeGuestNIC(t, func(int32) []net.Addr { return []net.Addr{linkLocal} })

	savedInterval, savedWindow := guestInfoRetryInterval, guestInfoRetryWindow
	guestInfoRetryInterval, guestInfoRetryWindow = 5*time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() { guestInfoRetryInterval, guestInfoRetryWindow = savedInterval, savedWindow })

	out := &recordingChannelOut{reply: rpciOK}
	s := NewService(new(mockChannelIn), out)
	s.GuestInfoInterval = time.Hour

	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	// Without an IPv4 address the retry runs for its window, then stops.
	time.Sleep(150 * time.Millisecond)
	ended := queries.Load()
	time.Sleep(100 * time.Millisecond)
	if n := queries.Load(); n != ended {
		t.Errorf("gathers continued after the retry window: %d queries, then %d", ended, n)
	}
	s.Stop()
	s.Wait()

	if ended < 2 {
		t.Errorf("%d gathers in the retry window, want retries", ended)
	}
}
