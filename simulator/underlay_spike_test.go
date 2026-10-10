// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

//go:build underlay_spike

package simulator

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/vmware/govmomi/find"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/vim25"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
)

// TestMain lets this test binary act as the netns helper: the runtime re-executes it inside a router namespace.
func TestMain(m *testing.M) {
	if served, err := ServeNetnsHelperFromEnv(); served {
		if err != nil {
			fmt.Fprintln(os.Stderr, "netns helper:", err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// Run with: go test -tags underlay_spike -run TestUnderlaySpike -v ./simulator/
// Needs rootless podman, a local busybox:latest, and the ip, sysctl and nft binaries on the host.
func TestUnderlaySpike(t *testing.T) {
	run := "spk" + strconv.FormatInt(time.Now().UnixNano()%100000000, 10)
	topo := &Topology{
		Run: run,
		Underlays: []Underlay{
			{Name: "wl", MTU: 1400},
			{Name: "transit"},
		},
		Routers: []Router{
			{Name: "r1", Forward: true,
				Interfaces: []RouterInterface{
					{Domain: "wl/v10", Address: "10.10.0.1/24"},
					{Domain: "transit/untagged", Address: "10.255.0.1/30", MTU: 1300},
				},
				Routes: []Route{{Dest: "10.20.0.0/24", Via: "10.255.0.2"}}},
			{Name: "r2", Forward: true,
				Interfaces: []RouterInterface{
					{Domain: "wl/v20", Address: "10.20.0.1/24"},
					{Domain: "transit/untagged", Address: "10.255.0.2/30"},
				},
				Routes: []Route{{Dest: "10.10.0.0/24", Via: "10.255.0.1"}}},
		},
	}

	// execIn runs a command in one of this run's guest containers and returns its trimmed output.
	execIn := func(name string, args ...string) (string, error) {
		return podmanOutput(append([]string{"exec", run + "-" + name}, args...)...)
	}

	rt, err := newUnderlayRuntime(topo)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rt.Close() })

	// Two VLAN domains give two networks, and a repeated request reuses the first.
	n10, err := rt.Network(Domain{Underlay: "wl", VLAN: 10})
	if err != nil {
		t.Fatal(err)
	}
	n20, err := rt.Network(Domain{Underlay: "wl", VLAN: 20})
	if err != nil {
		t.Fatal(err)
	}
	if n10 == n20 {
		t.Fatalf("vlan 10 and vlan 20 share network %s", n10)
	}
	if again, _ := rt.Network(Domain{Underlay: "wl", VLAN: 10}); again != n10 {
		t.Errorf("repeat Network(v10) = %s, want %s", again, n10)
	}
	if n := spikeLabelled(t, "network", run); n != 2 {
		t.Errorf("labelled networks after two domains: %d, want 2", n)
	}

	for _, r := range []string{"r1", "r2"} {
		if err := rt.StartRouter(r); err != nil {
			t.Fatal(err)
		}
	}
	if err := rt.StartRouter("r1"); err != nil {
		t.Errorf("repeat StartRouter(r1): %v", err)
	}

	// Per-link MTU on a router interface, set from outside the container.
	link, err := nsenter(rt.routers["r1"].pid, "", "ip", "-o", "link", "show", "r1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(link, "mtu 1300") {
		t.Errorf("router r1 interface r1: %s, want mtu 1300", link)
	}

	// Guests: c1 and c1b share v10 with their vNIC MACs, c2 sits on v20 behind r2.
	spikeGuest(t, run, "c1", n10, "02:00:00:00:10:01", "10.10.0.2/24", "")
	spikeGuest(t, run, "c1b", n10, "02:00:00:00:10:02", "10.10.0.3/24", "")
	spikeGuest(t, run, "c2", n20, "02:00:00:00:20:01", "10.20.0.2/24", "10.20.0.1")

	t.Run("mac", func(t *testing.T) {
		got := spikeExec(t, run, "c1", "cat", "/sys/class/net/eth0/address")
		if got != "02:00:00:00:10:01" {
			t.Errorf("eth0 mac %s, want 02:00:00:00:10:01", got)
		}
	})

	t.Run("same domain ping", func(t *testing.T) {
		if _, err := execIn("c1", "ping", "-c", "1", "-W", "2", "10.10.0.3"); err != nil {
			t.Errorf("c1 -> c1b: %v", err)
		}
	})

	t.Run("cross domain blocked", func(t *testing.T) {
		if _, err := execIn("c1", "ping", "-c", "1", "-W", "2", "10.20.0.2"); err == nil {
			t.Errorf("c1 -> c2 (v20) succeeded across domains")
		}
	})

	// HTTP server listening through the D3 hand-off, inside r1's namespace.
	l, err := rt.Listen("r1", "10.10.0.1:8080")
	if err != nil {
		t.Fatalf("listen hand-off: %v", err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "r1-ok")
	})}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })

	t.Run("listen on-subnet", func(t *testing.T) {
		got, err := execIn("c1", "wget", "-T", "3", "-qO-", "http://10.10.0.1:8080/")
		if err != nil || got != "r1-ok" {
			t.Errorf("c1 -> r1 listener: %q, %v", got, err)
		}
	})

	t.Run("listen routed through two routers", func(t *testing.T) {
		got, err := execIn("c2", "wget", "-T", "3", "-qO-", "http://10.10.0.1:8080/")
		if err != nil || got != "r1-ok" {
			t.Errorf("c2 -> r2 -> r1 listener: %q, %v", got, err)
		}
	})

	// httpd in c1b, reached by the host through a dial hand-off from r1.
	spikeStartHTTPD(t, run, "c1b")
	t.Run("dial hand-off reaches container", func(t *testing.T) {
		conn, err := rt.Dial("r1", "10.10.0.3:8000")
		if err != nil {
			t.Fatalf("dial hand-off: %v", err)
		}
		defer func() { _ = conn.Close() }()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		if _, err := io.WriteString(conn, "GET / HTTP/1.0\r\nHost: c1b\r\n\r\n"); err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(bufio.NewReader(conn))
		if !strings.Contains(string(body), "dial-ok") {
			t.Errorf("dial body %q, want dial-ok", body)
		}
	})

	t.Run("partition blocks and lift restores", func(t *testing.T) {
		lift, err := rt.Partition("r2", []string{"10.20.0.2"}, []string{"10.10.0.1"})
		if err != nil {
			t.Fatal(err)
		}
		if got, err := execIn("c2", "wget", "-T", "2", "-qO-", "http://10.10.0.1:8080/"); err == nil {
			t.Errorf("partitioned c2 -> r1 succeeded: %q", got)
		}
		if err := lift(); err != nil {
			t.Fatal(err)
		}
		if got, err := execIn("c2", "wget", "-T", "3", "-qO-", "http://10.10.0.1:8080/"); err != nil || got != "r1-ok" {
			t.Errorf("after lift c2 -> r1: %q, %v", got, err)
		}
	})

	t.Run("sweep removes dead owners only", func(t *testing.T) {
		dead := exec.Command("true")
		if err := dead.Run(); err != nil {
			t.Fatal(err)
		}
		deadPID := dead.Process.Pid
		if pidAlive(deadPID) {
			t.Skipf("pid %d reused", deadPID)
		}
		deadNet := "spk-dead-" + run
		spikePodman(t, "network", "create", "--internal", "--ipam-driver", "none",
			"--label", labelRun+"=dead", "--label", labelPID+"="+strconv.Itoa(deadPID), deadNet)
		if err := sweepStale(); err != nil {
			t.Fatal(err)
		}
		if spikeExists(t, "network", deadNet) {
			t.Errorf("dead owner's network %s survived the sweep", deadNet)
		}
		if n := spikeLabelled(t, "network", run); n != 3 {
			t.Errorf("live run's networks after sweep: %d, want 3", n)
		}
	})

	if err := rt.Close(); err != nil {
		t.Errorf("close: %v", err)
	}
	if n := spikeLabelled(t, "container", run) + spikeLabelled(t, "network", run); n != 0 {
		t.Errorf("labelled objects left after teardown: %d", n)
	}
}

// spikeGuest starts a busybox container on network with the given MAC, configures eth0, and sets a default
// route if gw is given. Containers carry the run labels so Close removes them.
func spikeGuest(t *testing.T, run, name, network, mac, addr, gw string) {
	t.Helper()
	// NET_RAW lets busybox ping open its raw socket; the guest is otherwise unprivileged.
	spikePodman(t, "run", "-d", "--pull=never", "--name", run+"-"+name, "--cap-add", "NET_RAW",
		"--label", labelRun+"="+run, "--label", labelPID+"="+strconv.Itoa(os.Getpid()),
		"--network", network+":mac="+mac+",interface_name=eth0",
		routerImage, "sleep", "2147483647")
	pid, err := strconv.Atoi(spikePodman(t, "inspect", "--format", "{{.State.Pid}}", run+"-"+name))
	if err != nil {
		t.Fatal(err)
	}
	steps := [][]string{{"ip", "addr", "add", addr, "dev", "eth0"}, {"ip", "link", "set", "dev", "eth0", "up"}}
	if gw != "" {
		steps = append(steps, []string{"ip", "route", "add", "default", "via", gw})
	}
	for _, s := range steps {
		if _, err := nsenter(pid, "", s...); err != nil {
			t.Fatal(err)
		}
	}
}

// spikeStartHTTPD serves "dial-ok" on port 8000 from a busybox container, in a second container on the network.
func spikeStartHTTPD(t *testing.T, run, name string) {
	t.Helper()
	spikePodman(t, "exec", "-d", run+"-"+name, "sh", "-c", "mkdir -p /www && echo dial-ok > /www/index.html && httpd -f -p 8000 -h /www")
}

func spikePodman(t *testing.T, args ...string) string {
	t.Helper()
	out, err := podmanOutput(args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func spikeExec(t *testing.T, run, name string, args ...string) string {
	t.Helper()
	return spikePodman(t, append([]string{"exec", run + "-" + name}, args...)...)
}

func spikeLabelled(t *testing.T, kind, run string) int {
	t.Helper()
	var out string
	var err error
	if kind == "network" {
		out, err = podmanOutput("network", "ls", "--filter", "label="+labelRun+"="+run, "--format", "{{.Name}}")
	} else {
		out, err = podmanOutput("ps", "-a", "--filter", "label="+labelRun+"="+run, "--format", "{{.ID}}")
	}
	if err != nil {
		t.Fatal(err)
	}
	if out == "" {
		return 0
	}
	return len(strings.Split(out, "\n"))
}

func spikeExists(t *testing.T, kind, name string) bool {
	t.Helper()
	ok, err := podmanExists(kind, "exists", name)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

// TestUnderlaySpikeVMs runs container VMs from the simulator on one underlay. Each VM's vNIC joins its DVPG's
// domain through the topology, and the guest is configured by MAC from outside the container. It covers
// same-domain ping, the MAC and MTU in the guest, guest.net by MAC, live vNIC add and remove, the connected
// toggle, a backing change to another VLAN, startConnected=false, and a router with forward:false.
// Run with: go test -tags underlay_spike -run TestUnderlaySpikeVMs -v ./simulator/
func TestUnderlaySpikeVMs(t *testing.T) {
	run := "spv" + strconv.FormatInt(time.Now().UnixNano()%100000000, 10)
	topo := &Topology{
		Run:       run,
		Underlays: []Underlay{{Name: "wl", MTU: 1400}},
		Attachments: []Attachment{
			{Host: "*", Pnic: "vmnic0", Underlay: "wl"},
			{Host: "*", Pnic: "vmnic1", Underlay: "wl"},
		},
		Routers: []Router{
			// r3 does not forward. Its two domains are otherwise fully routed to each other by the guests' default routes.
			{Name: "r3", Forward: false, Interfaces: []RouterInterface{
				{Domain: "wl/v30", Address: "10.30.0.1/24"},
				{Domain: "wl/v40", Address: "10.40.0.1/24"},
			}},
			// r4 is the control: the same shape with forwarding on.
			{Name: "r4", Forward: true, Interfaces: []RouterInterface{
				{Domain: "wl/v50", Address: "10.50.0.1/24"},
				{Domain: "wl/v60", Address: "10.60.0.1/24"},
			}},
		},
	}

	m := VPX()
	m.Autostart = false
	m.Machine = 0
	m.Topology = topo

	err := m.Run(func(ctx context.Context, c *vim25.Client) error {
		spikeVMScenario(t, ctx, c, m, run)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if n := spikeLabelled(t, "container", run) + spikeLabelled(t, "network", run); n != 0 {
		t.Errorf("labelled objects left after Model.Remove: %d", n)
	}
}

// spikeVM is a container VM of the scenario: its MOR, its device MAC and its container ID.
type spikeVM struct {
	name string
	vm   *object.VirtualMachine
	mac  string
	id   string
}

func spikeVMScenario(t *testing.T, ctx context.Context, c *vim25.Client, m *Model, run string) {
	finder := find.NewFinder(c)
	dc, err := finder.Datacenter(ctx, "DC0")
	if err != nil {
		t.Fatal(err)
	}
	finder.SetDatacenter(dc)
	pool, err := finder.ResourcePool(ctx, "DC0_H0/Resources")
	if err != nil {
		t.Fatal(err)
	}
	folders, err := dc.Folders(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// One DVPG per VLAN, on the Model's DVS.
	vlans := []int32{10, 20, 30, 40, 50, 60}
	pg0, err := finder.Network(ctx, "DC0_DVPG0")
	if err != nil {
		t.Fatal(err)
	}
	var pgMo mo.DistributedVirtualPortgroup
	if err := object.NewDistributedVirtualPortgroup(c, pg0.Reference()).Properties(ctx, pg0.Reference(), []string{"config"}, &pgMo); err != nil {
		t.Fatal(err)
	}
	dvsRef := *pgMo.Config.DistributedVirtualSwitch
	var dvsMo mo.DistributedVirtualSwitch
	dvs := object.NewDistributedVirtualSwitch(c, dvsRef)
	if err := dvs.Properties(ctx, dvsRef, []string{"uuid"}, &dvsMo); err != nil {
		t.Fatal(err)
	}

	var specs []types.DVPortgroupConfigSpec
	for _, v := range vlans {
		specs = append(specs, types.DVPortgroupConfigSpec{
			Name:     fmt.Sprintf("%s-v%d", run, v),
			Type:     "earlyBinding",
			NumPorts: 8,
			DefaultPortConfig: &types.VMwareDVSPortSetting{
				Vlan: &types.VmwareDistributedVirtualSwitchVlanIdSpec{VlanId: v},
			},
		})
	}
	task, err := dvs.AddPortgroup(ctx, specs)
	if err != nil {
		t.Fatal(err)
	}
	if err := task.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	pgs := make(map[int32]types.ManagedObjectReference)
	for _, v := range vlans {
		n, err := finder.Network(ctx, fmt.Sprintf("%s-v%d", run, v))
		if err != nil {
			t.Fatal(err)
		}
		pgs[v] = n.Reference()
	}

	nic := func(vlan int32, connectable *types.VirtualDeviceConnectInfo) types.BaseVirtualDeviceConfigSpec {
		return &types.VirtualDeviceConfigSpec{
			Operation: types.VirtualDeviceConfigSpecOperationAdd,
			Device: &types.VirtualVmxnet3{VirtualVmxnet: types.VirtualVmxnet{VirtualEthernetCard: types.VirtualEthernetCard{
				VirtualDevice: types.VirtualDevice{
					Key: -1,
					Backing: &types.VirtualEthernetCardDistributedVirtualPortBackingInfo{Port: types.DistributedVirtualSwitchPortConnection{
						PortgroupKey: pgs[vlan].Value,
						SwitchUuid:   dvsMo.Uuid,
					}},
					Connectable: connectable,
				},
			}}},
		}
	}
	connected := &types.VirtualDeviceConnectInfo{StartConnected: true, Connected: true, AllowGuestControl: true}

	// create makes a container VM with one vNIC on vlan and powers it on.
	create := func(name string, vlan int32, connectable *types.VirtualDeviceConnectInfo) spikeVM {
		spec := types.VirtualMachineConfigSpec{
			Name:  name,
			Files: &types.VirtualMachineFileInfo{VmPathName: "[LocalDS_0] " + name},
			ExtraConfig: []types.BaseOptionValue{
				&types.OptionValue{Key: ContainerBackingOptionKey, Value: `["busybox:latest","sleep","2147483647"]`},
				// Rootless podman cannot mount the DMI files.
				&types.OptionValue{Key: "RUN.mountdmi", Value: "false"},
			},
			DeviceChange: []types.BaseVirtualDeviceConfigSpec{nic(vlan, connectable)},
		}
		task, err := folders.VmFolder.CreateVM(ctx, spec, pool, nil)
		if err != nil {
			t.Fatal(err)
		}
		info, err := task.WaitForResult(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		vm := object.NewVirtualMachine(c, info.Result.(types.ManagedObjectReference))
		if task, err := vm.PowerOn(ctx); err != nil {
			t.Fatal(err)
		} else if err := task.Wait(ctx); err != nil {
			t.Fatalf("power on %s: %v", name, err)
		}
		v := spikeVM{name: name, vm: vm, id: spikeContainerID(t, m, vm.Reference())}
		if nics := spikeNICs(t, ctx, vm); len(nics) > 0 {
			v.mac = nics[0].(types.BaseVirtualEthernetCard).GetVirtualEthernetCard().MacAddress
		}
		return v
	}

	a := create("a", 10, connected)
	b := create("b", 10, connected)
	g := create("g", 20, connected)
	h := create("h", 10, connected)
	cc := create("c", 30, connected)
	d := create("d", 40, connected)
	e := create("e", 50, connected)
	f := create("f", 60, connected)
	j := create("j", 10, &types.VirtualDeviceConnectInfo{StartConnected: false, Connected: false, AllowGuestControl: true})

	// Each VM is addressed from outside the container: the guest's default route is through its router, where
	// the VM has one.
	spikeConfigure(t, a, "10.10.0.2/24", "")
	spikeConfigure(t, b, "10.10.0.3/24", "")
	spikeConfigure(t, g, "10.20.0.3/24", "")
	spikeConfigure(t, h, "10.10.0.4/24", "")
	spikeConfigure(t, cc, "10.30.0.2/24", "10.30.0.1")
	spikeConfigure(t, d, "10.40.0.2/24", "10.40.0.1")
	spikeConfigure(t, e, "10.50.0.2/24", "10.50.0.1")
	spikeConfigure(t, f, "10.60.0.2/24", "10.60.0.1")

	t.Run("mac and mtu", func(t *testing.T) {
		dev := spikeIface(t, a.id, a.mac)
		if dev == "" {
			t.Fatalf("no interface with vNIC MAC %s", a.mac)
		}
		pid := spikePid(t, a.id)
		link := spikeNsenter(t, pid, "ip", "-o", "link", "show", "dev", dev)
		if !strings.Contains(link, "mtu 1400") {
			t.Errorf("interface %s: %s, want mtu 1400 from the domain network", dev, link)
		}
	})

	t.Run("same domain ping", func(t *testing.T) {
		if err := spikePing(a.id, "10.10.0.3"); err != nil {
			t.Errorf("a -> b on v10: %v", err)
		}
	})

	t.Run("guest.net by MAC", func(t *testing.T) {
		n, ok := spikeGuestNIC(t, ctx, a.vm, a.mac)
		if !ok {
			t.Fatalf("guest.net has no entry for MAC %s", a.mac)
		}
		if !n.Connected {
			t.Errorf("guest.net entry for %s not connected", a.mac)
		}
		if want := run + "-v10"; n.Network != want {
			t.Errorf("guest.net network %q, want %q", n.Network, want)
		}
		if n.DeviceConfigId == 0 {
			t.Errorf("guest.net entry for %s has no device key", a.mac)
		}
	})

	t.Run("live add and remove", func(t *testing.T) {
		add := &types.VirtualDeviceConfigSpec{
			Operation: types.VirtualDeviceConfigSpecOperationAdd,
			Device:    nic(20, connected).GetVirtualDeviceConfigSpec().Device,
		}
		add.Device.GetVirtualDevice().Key = -10
		spikeReconfigure(t, ctx, a.vm, add)

		nics := spikeNICs(t, ctx, a.vm)
		if len(nics) != 2 {
			t.Fatalf("a has %d vNICs after add, want 2", len(nics))
		}
		mac2 := nics[1].(types.BaseVirtualEthernetCard).GetVirtualEthernetCard().MacAddress
		if spikeIface(t, a.id, mac2) == "" {
			t.Fatalf("no interface with added vNIC MAC %s", mac2)
		}
		spikeConfigureIface(t, a.id, mac2, "10.20.0.2/24", "")
		if err := spikePing(a.id, "10.20.0.3"); err != nil {
			t.Errorf("a -> g on v20 through the added vNIC: %v", err)
		}
		if n := len(spikeGuestNICs(t, ctx, a.vm)); n != 2 {
			t.Errorf("guest.net entries after add: %d, want 2", n)
		}

		spikeReconfigure(t, ctx, a.vm, &types.VirtualDeviceConfigSpec{
			Operation: types.VirtualDeviceConfigSpecOperationRemove,
			Device:    nics[1],
		})
		if dev := spikeIface(t, a.id, mac2); dev != "" {
			t.Errorf("interface %s with removed vNIC MAC %s survived the remove", dev, mac2)
		}
		if n := len(spikeGuestNICs(t, ctx, a.vm)); n != 1 {
			t.Errorf("guest.net entries after remove: %d, want 1", n)
		}
	})

	t.Run("connected toggle", func(t *testing.T) {
		setConnected := func(on bool) {
			dev := spikeNICs(t, ctx, a.vm)[0]
			dev.(types.BaseVirtualEthernetCard).GetVirtualEthernetCard().Connectable.Connected = on
			spikeReconfigure(t, ctx, a.vm, &types.VirtualDeviceConfigSpec{
				Operation: types.VirtualDeviceConfigSpecOperationEdit,
				Device:    dev,
			})
		}

		setConnected(false)
		if dev := spikeIface(t, a.id, a.mac); dev != "" {
			t.Errorf("disconnected vNIC still has interface %s", dev)
		}
		if err := spikePing(a.id, "10.10.0.3"); err == nil {
			t.Errorf("a -> b succeeded while a's vNIC is disconnected")
		}
		if n, ok := spikeGuestNIC(t, ctx, a.vm, a.mac); !ok || n.Connected {
			t.Errorf("guest.net for disconnected vNIC: found %v connected %v, want found and not connected", ok, n.Connected)
		}

		setConnected(true)
		// podman gives the reconnected interface a new name, so the guest is found by MAC again.
		if dev := spikeIface(t, a.id, a.mac); dev == "" {
			t.Fatalf("reconnected vNIC has no interface with MAC %s", a.mac)
		}
		spikeConfigure(t, a, "10.10.0.2/24", "")
		if err := spikePing(a.id, "10.10.0.3"); err != nil {
			t.Errorf("a -> b after reconnect: %v", err)
		}
		if n, ok := spikeGuestNIC(t, ctx, a.vm, a.mac); !ok || !n.Connected {
			t.Errorf("guest.net after reconnect: found %v connected %v", ok, n.Connected)
		}
	})

	t.Run("backing change to another vlan", func(t *testing.T) {
		dev := spikeNICs(t, ctx, h.vm)[0]
		card := dev.(types.BaseVirtualEthernetCard).GetVirtualEthernetCard()
		card.Backing = &types.VirtualEthernetCardDistributedVirtualPortBackingInfo{Port: types.DistributedVirtualSwitchPortConnection{
			PortgroupKey: pgs[20].Value,
			SwitchUuid:   dvsMo.Uuid,
		}}
		spikeReconfigure(t, ctx, h.vm, &types.VirtualDeviceConfigSpec{
			Operation: types.VirtualDeviceConfigSpecOperationEdit,
			Device:    dev,
		})

		if spikeIface(t, h.id, h.mac) == "" {
			t.Fatalf("moved vNIC has no interface with MAC %s", h.mac)
		}
		// The address on v10 must not work any more, so h gets its v20 address and then also its old v10 one.
		spikeConfigureIface(t, h.id, h.mac, "10.20.0.4/24", "")
		if err := spikePing(h.id, "10.20.0.3"); err != nil {
			t.Errorf("h -> g on v20 after move: %v", err)
		}
		spikeConfigureIface(t, h.id, h.mac, "10.10.0.4/24", "")
		if err := spikePing(h.id, "10.10.0.2"); err == nil {
			t.Errorf("h -> a on v10 succeeded after h moved to v20")
		}
		if n, ok := spikeGuestNIC(t, ctx, h.vm, h.mac); !ok || n.Network != run+"-v20" {
			t.Errorf("guest.net after move: found %v network %q, want %q", ok, n.Network, run+"-v20")
		}
	})

	t.Run("start disconnected", func(t *testing.T) {
		if spikeIface(t, j.id, j.mac) != "" {
			t.Errorf("startConnected=false vNIC got an interface at power-on")
		}
		if n, ok := spikeGuestNIC(t, ctx, j.vm, j.mac); !ok || n.Connected {
			t.Errorf("guest.net for startConnected=false vNIC: found %v connected %v", ok, n.Connected)
		}
	})

	t.Run("forward false does not forward", func(t *testing.T) {
		// Both routes are in place: each guest reaches its router, and the control pair on r4 forwards.
		if err := spikePing(cc.id, "10.30.0.1"); err != nil {
			t.Fatalf("c -> r3 gateway: %v (the route under test is not in place)", err)
		}
		if err := spikePing(e.id, "10.60.0.2"); err != nil {
			t.Errorf("control e -> f through r4 forward:true: %v", err)
		}
		if err := spikePing(cc.id, "10.40.0.2"); err == nil {
			t.Errorf("c -> d across r3 succeeded with forward:false")
		}
	})
}

// spikeContainerID returns the container ID behind a container VM.
func spikeContainerID(t *testing.T, m *Model, ref types.ManagedObjectReference) string {
	t.Helper()
	vm, ok := m.Service.Context.Map.Get(ref).(*VirtualMachine)
	if !ok || vm.svm == nil || vm.svm.c == nil || vm.svm.c.id == "" {
		t.Fatalf("%s: no container", ref.Value)
	}
	return vm.svm.c.id
}

// spikeNICs returns a VM's vNICs in device list order.
func spikeNICs(t *testing.T, ctx context.Context, vm *object.VirtualMachine) []types.BaseVirtualDevice {
	t.Helper()
	devs, err := vm.Device(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var nics []types.BaseVirtualDevice
	for _, d := range devs {
		if _, ok := d.(types.BaseVirtualEthernetCard); ok {
			nics = append(nics, d)
		}
	}
	return nics
}

// spikeReconfigure applies one device change to vm and waits for the task.
func spikeReconfigure(t *testing.T, ctx context.Context, vm *object.VirtualMachine, change types.BaseVirtualDeviceConfigSpec) {
	t.Helper()
	task, err := vm.Reconfigure(ctx, types.VirtualMachineConfigSpec{DeviceChange: []types.BaseVirtualDeviceConfigSpec{change}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := task.WaitForResult(ctx, nil); err != nil {
		t.Fatalf("reconfigure %s: %v", vm.Reference().Value, err)
	}
}

// spikeGuestNICs returns a VM's guest.net.
func spikeGuestNICs(t *testing.T, ctx context.Context, vm *object.VirtualMachine) []types.GuestNicInfo {
	t.Helper()
	var o mo.VirtualMachine
	if err := vm.Properties(ctx, vm.Reference(), []string{"guest.net"}, &o); err != nil {
		t.Fatal(err)
	}
	return o.Guest.Net
}

// spikeGuestNIC returns the guest.net entry with mac.
func spikeGuestNIC(t *testing.T, ctx context.Context, vm *object.VirtualMachine, mac string) (types.GuestNicInfo, bool) {
	t.Helper()
	for _, n := range spikeGuestNICs(t, ctx, vm) {
		if strings.EqualFold(n.MacAddress, mac) {
			return n, true
		}
	}
	return types.GuestNicInfo{}, false
}

// spikePid returns the PID of the container's namespace owner.
func spikePid(t *testing.T, id string) int {
	t.Helper()
	pid, err := strconv.Atoi(spikePodman(t, "inspect", "--format", "{{.State.Pid}}", id))
	if err != nil {
		t.Fatal(err)
	}
	return pid
}

// spikeNsenter runs a command in the container's network namespace and returns its output.
func spikeNsenter(t *testing.T, pid int, args ...string) string {
	t.Helper()
	out, err := nsenter(pid, "", args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// spikeIface returns the name of the container's interface with mac, or "" if there is none.
func spikeIface(t *testing.T, id, mac string) string {
	t.Helper()
	out := spikePodman(t, "exec", id, "sh", "-c", "for i in /sys/class/net/*; do echo $(basename $i) $(cat $i/address); done")
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && strings.EqualFold(f[1], mac) {
			return f[0]
		}
	}
	return ""
}

// spikeConfigure addresses the container's interface with the MAC of the spike VM v, and sets a default route if gw is given.
func spikeConfigure(t *testing.T, v spikeVM, addr, gw string) {
	t.Helper()
	spikeConfigureIface(t, v.id, v.mac, addr, gw)
}

// spikeConfigureIface addresses the interface with mac from outside the container: exec lacks NET_ADMIN.
func spikeConfigureIface(t *testing.T, id, mac, addr, gw string) {
	t.Helper()
	dev := spikeIface(t, id, mac)
	if dev == "" {
		t.Fatalf("no interface with MAC %s in %s", mac, id)
	}
	pid := spikePid(t, id)
	steps := [][]string{{"ip", "link", "set", "dev", dev, "up"}, {"ip", "addr", "add", addr, "dev", dev}}
	if gw != "" {
		steps = append(steps, []string{"ip", "route", "replace", "default", "via", gw})
	}
	for _, s := range steps {
		spikeNsenter(t, pid, s...)
	}
}

// spikePing pings addr once from the container, returning an error when there is no reply.
func spikePing(id, addr string) error {
	_, err := podmanOutput("exec", id, "ping", "-c", "1", "-W", "2", addr)
	return err
}
