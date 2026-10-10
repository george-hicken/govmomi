// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

//go:build underlay_spike

package simulator

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
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
