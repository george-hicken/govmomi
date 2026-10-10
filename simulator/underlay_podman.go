// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package simulator

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// labelRun and labelPID tag every podman object the underlay creates, so a later run can find and remove
	// what a crashed run left behind.
	labelRun = "vcsim.run"
	labelPID = "vcsim.pid"

	// routerImage must be present locally: routers are started with --pull=never.
	routerImage = "busybox:latest"

	helperStartTimeout = 10 * time.Second
	helperStopTimeout  = 5 * time.Second
)

// underlayRuntime realises one Topology in podman. Domain networks are created lazily and idempotently,
// routers are containers joined to their domains, and everything carries the run's labels.
type underlayRuntime struct {
	mu sync.Mutex

	topo *Topology
	run  string
	pid  int

	networks map[string]bool // podman network names created or found by this runtime
	routers  map[string]*routerInstance
	faults   int
}

// routerInstance is a started router: its container, its namespace PID, and its netns helper once one is running.
type routerInstance struct {
	name      string
	container string
	pid       int

	helper    *netnsHelper
	helperCmd *exec.Cmd
	helperDir string
	helperErr chan error
}

// newUnderlayRuntime sweeps labelled objects left by dead processes, then returns a runtime for t.
// It must run before the first network or router is created.
func newUnderlayRuntime(t *Topology) (*underlayRuntime, error) {
	if err := t.Validate(); err != nil {
		return nil, err
	}

	if err := sweepStale(); err != nil {
		return nil, fmt.Errorf("sweep stale underlay objects: %w", err)
	}

	return &underlayRuntime{
		topo:     t,
		run:      t.Run,
		pid:      os.Getpid(),
		networks: make(map[string]bool),
		routers:  make(map[string]*routerInstance),
	}, nil
}

// Network returns the podman network for a domain, creating it on first use. Repeated calls are no-ops.
func (u *underlayRuntime) Network(d Domain) (string, error) {
	u.mu.Lock()
	defer u.mu.Unlock()

	return u.ensureNetwork(d)
}

// ensureNetwork is Network for callers that already hold u.mu.
func (u *underlayRuntime) ensureNetwork(d Domain) (string, error) {
	name := d.networkName(u.run)
	if u.networks[name] {
		return name, nil
	}

	exists, err := podmanExists("network", "exists", name)
	if err != nil {
		return "", err
	}
	if !exists {
		args := []string{"network", "create", "--internal", "--ipam-driver", "none",
			"--label", u.runLabel(), "--label", u.pidLabel()}
		if mtu := u.underlayMTU(d.Underlay); mtu > 0 {
			args = append(args, "--opt", "mtu="+strconv.Itoa(mtu))
		}
		args = append(args, name)
		if _, err := podmanOutput(args...); err != nil {
			return "", err
		}
	}

	u.networks[name] = true
	return name, nil
}

// StartRouter creates the named router, joins it to its domains and configures its addresses, MTUs, forwarding
// and routes from outside the container. A router already started by this runtime is left alone.
func (u *underlayRuntime) StartRouter(name string) error {
	u.mu.Lock()
	defer u.mu.Unlock()

	if _, ok := u.routers[name]; ok {
		return nil
	}

	var spec *Router
	for i := range u.topo.Routers {
		if u.topo.Routers[i].Name == name {
			spec = &u.topo.Routers[i]
		}
	}
	if spec == nil {
		return fmt.Errorf("router %q: not in topology", name)
	}

	cname := u.run + "-rtr-" + name
	exists, err := podmanExists("container", "exists", cname)
	if err != nil {
		return err
	}
	if exists {
		return fmt.Errorf("router %q: container %s exists but was not started by this run", name, cname)
	}

	args := []string{"run", "-d", "--pull=never", "--name", cname,
		"--label", u.runLabel(), "--label", u.pidLabel()}
	for i, ifc := range spec.Interfaces {
		d, err := parseDomain(ifc.Domain)
		if err != nil {
			return err
		}
		netName, err := u.ensureNetwork(d)
		if err != nil {
			return err
		}
		args = append(args, "--network", fmt.Sprintf("%s:interface_name=r%d", netName, i))
	}
	args = append(args, routerImage, "sleep", "2147483647")
	if _, err := podmanOutput(args...); err != nil {
		return err
	}

	out, err := podmanOutput("inspect", "--format", "{{.State.Pid}}", cname)
	if err != nil {
		return err
	}
	pid, err := strconv.Atoi(out)
	if err != nil {
		return fmt.Errorf("router %q: pid %q: %w", name, out, err)
	}

	rt := &routerInstance{name: name, container: cname, pid: pid}
	if err := u.configureRouter(rt, spec); err != nil {
		return err
	}

	u.routers[name] = rt
	return nil
}

// configureRouter sets interface addresses and MTUs, enables forwarding if requested, and adds routes.
// Each step runs through `podman unshare nsenter` so the host's ip and sysctl binaries act in the
// router's namespace.
func (u *underlayRuntime) configureRouter(rt *routerInstance, spec *Router) error {
	steps := [][]string{{"ip", "link", "set", "dev", "lo", "up"}}
	for i, ifc := range spec.Interfaces {
		dev := fmt.Sprintf("r%d", i)
		steps = append(steps, []string{"ip", "link", "set", "dev", dev, "up"})
		if ifc.MTU > 0 {
			steps = append(steps, []string{"ip", "link", "set", "dev", dev, "mtu", strconv.Itoa(ifc.MTU)})
		}
		steps = append(steps, []string{"ip", "addr", "add", ifc.Address, "dev", dev})
		for _, alias := range ifc.Aliases {
			steps = append(steps, []string{"ip", "addr", "add", alias, "dev", dev})
		}
	}
	if spec.Forward {
		steps = append(steps, []string{"sysctl", "-q", "-w", "net.ipv4.ip_forward=1"})
	}
	for _, r := range spec.Routes {
		steps = append(steps, []string{"ip", "route", "add", r.Dest, "via", r.Via})
	}

	for _, s := range steps {
		if _, err := nsenter(rt.pid, "", s...); err != nil {
			return fmt.Errorf("router %q: %w", rt.name, err)
		}
	}
	return nil
}

// Listen returns a listener bound to addr inside the named router's namespace, reached through its netns helper.
func (u *underlayRuntime) Listen(router, addr string) (net.Listener, error) {
	h, err := u.helperFor(router)
	if err != nil {
		return nil, err
	}
	return h.Listen(addr)
}

// Dial returns a connection from the named router's namespace to addr, which may be on a domain only the router sees.
func (u *underlayRuntime) Dial(router, addr string) (net.Conn, error) {
	h, err := u.helperFor(router)
	if err != nil {
		return nil, err
	}
	return h.Dial(addr)
}

// ErrNoTopology is returned by Model.ListenRouter and Model.DialRouter when the Model has no Topology.
var ErrNoTopology = errors.New("no underlay topology loaded")

// ListenRouter returns a listener on addr in the namespace of the named router of the Model's Topology. It is how
// an endpoint on a router's domain, such as vCenter on the management subnet, is served; see Listen.
func (m *Model) ListenRouter(router, addr string) (net.Listener, error) {
	u, err := m.underlay()
	if err != nil {
		return nil, err
	}
	return u.Listen(router, addr)
}

// DialRouter returns a connection from the named router's namespace to addr; see Dial.
func (m *Model) DialRouter(router, addr string) (net.Conn, error) {
	u, err := m.underlay()
	if err != nil {
		return nil, err
	}
	return u.Dial(router, addr)
}

// underlay returns the Model's underlay runtime, or ErrNoTopology before Create has realised a Topology.
func (m *Model) underlay() (*underlayRuntime, error) {
	if m.Service == nil || m.Service.Context == nil || m.Service.Context.Map.underlay == nil {
		return nil, ErrNoTopology
	}
	return m.Service.Context.Map.underlay, nil
}

// helperFor returns the router's netns helper, starting it on first use.
func (u *underlayRuntime) helperFor(router string) (*netnsHelper, error) {
	u.mu.Lock()
	defer u.mu.Unlock()

	rt, ok := u.routers[router]
	if !ok {
		return nil, fmt.Errorf("router %q: not started", router)
	}
	if rt.helper != nil {
		return rt.helper, nil
	}

	if err := u.startHelper(rt); err != nil {
		return nil, err
	}
	return rt.helper, nil
}

// startHelper runs a copy of this binary inside the router's namespace and waits until it accepts connections.
func (u *underlayRuntime) startHelper(rt *routerInstance) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}

	dir, err := os.MkdirTemp("", "vcsim-netns-")
	if err != nil {
		return err
	}
	sock := filepath.Join(dir, "helper.sock")

	cmd := exec.Command("podman", "unshare", "nsenter", "-t", strconv.Itoa(rt.pid), "-n", exe)
	cmd.Env = append(os.Environ(), netnsHelperEnv+"="+sock, netnsOwnerEnv+"="+strconv.Itoa(u.pid))
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		_ = os.RemoveAll(dir)
		return err
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	deadline := time.Now().Add(helperStartTimeout)
	for {
		if c, err := net.Dial("unix", sock); err == nil {
			_ = c.Close()
			break
		}
		select {
		case err := <-done:
			_ = os.RemoveAll(dir)
			return fmt.Errorf("router %q: netns helper exited: %v: %s", rt.name, err, strings.TrimSpace(stderr.String()))
		default:
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			_ = os.RemoveAll(dir)
			return fmt.Errorf("router %q: netns helper did not start", rt.name)
		}
		time.Sleep(20 * time.Millisecond)
	}

	rt.helper = &netnsHelper{socket: sock}
	rt.helperCmd = cmd
	rt.helperDir = dir
	rt.helperErr = done
	return nil
}

// stopHelper asks a router's helper to exit, kills it if it does not, and removes its socket directory.
func (rt *routerInstance) stopHelper() error {
	if rt.helper == nil {
		return nil
	}

	_ = rt.helper.shutdown()
	var err error
	select {
	case <-rt.helperErr:
	case <-time.After(helperStopTimeout):
		err = rt.helperCmd.Process.Kill()
		<-rt.helperErr
	}
	_ = os.RemoveAll(rt.helperDir)
	rt.helper = nil
	return err
}

// Partition drops forwarded traffic between the addresses in a and those in b, in both directions, at the
// named router, until the returned lift function is called. Traffic that does not cross the router is unaffected.
func (u *underlayRuntime) Partition(router string, a, b []string) (func() error, error) {
	var pairs [][2]string
	for _, x := range a {
		for _, y := range b {
			pairs = append(pairs, [2]string{x, y}, [2]string{y, x})
		}
	}
	return u.drop(router, pairs)
}

// Drop drops forwarded traffic from src to dst at the named router until the returned lift function is called.
func (u *underlayRuntime) Drop(router, src, dst string) (func() error, error) {
	return u.drop(router, [][2]string{{src, dst}})
}

func (u *underlayRuntime) drop(router string, pairs [][2]string) (func() error, error) {
	u.mu.Lock()
	defer u.mu.Unlock()

	rt, ok := u.routers[router]
	if !ok {
		return nil, fmt.Errorf("router %q: not started", router)
	}

	for _, p := range pairs {
		for _, addr := range p {
			if _, err := netip.ParseAddr(addr); err != nil {
				return nil, fmt.Errorf("fault address %q: %w", addr, err)
			}
		}
	}

	u.faults++
	table := fmt.Sprintf("vcsim_fault_%d", u.faults)

	var b strings.Builder
	fmt.Fprintf(&b, "table inet %s {\n\tchain forward {\n\t\ttype filter hook forward priority 0; policy accept;\n", table)
	for _, p := range pairs {
		fmt.Fprintf(&b, "\t\tip saddr %s ip daddr %s drop\n", p[0], p[1])
	}
	b.WriteString("\t}\n}\n")

	if _, err := nsenter(rt.pid, b.String(), "nft", "-f", "-"); err != nil {
		return nil, fmt.Errorf("router %q: install fault: %w", router, err)
	}

	pid := rt.pid
	return func() error {
		_, err := nsenter(pid, "", "nft", "delete", "table", "inet", table)
		return err
	}, nil
}

// Close stops the runtime's helpers, then removes every container and network labelled with its run.
// Order matters: a router's namespace stays alive while its helper runs, and a network in use cannot be removed.
func (u *underlayRuntime) Close() error {
	u.mu.Lock()
	defer u.mu.Unlock()

	var errs []error
	for name, rt := range u.routers {
		if err := rt.stopHelper(); err != nil {
			errs = append(errs, fmt.Errorf("router %q: stop helper: %w", name, err))
		}
	}
	u.routers = make(map[string]*routerInstance)

	errs = append(errs, removeLabelled("label="+u.runLabel(), func(map[string]string) bool { return true }))
	u.networks = make(map[string]bool)
	return errors.Join(errs...)
}

// runLabel and pidLabel are the labels every object of this runtime carries.
func (u *underlayRuntime) runLabel() string {
	return labelRun + "=" + u.run
}

func (u *underlayRuntime) pidLabel() string {
	return labelPID + "=" + strconv.Itoa(u.pid)
}

// underlayMTU returns the MTU configured for an underlay, or zero.
func (u *underlayRuntime) underlayMTU(name string) int {
	for _, x := range u.topo.Underlays {
		if x.Name == name {
			return x.MTU
		}
	}
	return 0
}

// sweepStale removes labelled containers and networks whose owning process is no longer running.
// Live objects, including those of other runs on this host, are left alone.
func sweepStale() error {
	return removeLabelled("label="+labelRun, func(labels map[string]string) bool {
		pid, err := strconv.Atoi(labels[labelPID])
		return err != nil || !pidAlive(pid)
	})
}

// labelledObject is the subset of `podman ps` and `podman network ls` JSON that removal needs.
type labelledObject struct {
	ID     string            `json:"Id"`
	Name   string            `json:"Name"`
	Names  []string          `json:"Names"`
	Labels map[string]string `json:"Labels"`
}

// removeLabelled removes containers, then networks, matching the podman filter and the predicate.
func removeLabelled(filter string, match func(labels map[string]string) bool) error {
	out, err := podmanOutput("ps", "-a", "--filter", filter, "--format", "json")
	if err != nil {
		return err
	}
	var containers []labelledObject
	if out != "" {
		if err := json.Unmarshal([]byte(out), &containers); err != nil {
			return fmt.Errorf("podman ps: %w", err)
		}
	}
	var errs []error
	for _, c := range containers {
		if match(c.Labels) {
			if _, err := podmanOutput("rm", "-f", "-t", "0", c.ID); err != nil {
				errs = append(errs, err)
			}
		}
	}

	out, err = podmanOutput("network", "ls", "--filter", filter, "--format", "json")
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	var networks []labelledObject
	if out != "" {
		if err := json.Unmarshal([]byte(out), &networks); err != nil {
			return errors.Join(append(errs, fmt.Errorf("podman network ls: %w", err))...)
		}
	}
	for _, n := range networks {
		if match(n.Labels) {
			if _, err := podmanOutput("network", "rm", "-f", n.Name); err != nil {
				errs = append(errs, err)
			}
		}
	}

	return errors.Join(errs...)
}

// nsenter runs args in the network namespace of pid, as rootless podman's owner namespace requires.
func nsenter(pid int, stdin string, args ...string) (string, error) {
	cmdArgs := append([]string{"unshare", "nsenter", "-t", strconv.Itoa(pid), "-n"}, args...)
	cmd := exec.Command("podman", cmdArgs...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("podman %v: %w: %s", cmdArgs, err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(string(out)), nil
}

// podmanOutput runs podman and returns trimmed stdout. Errors carry podman's stderr.
func podmanOutput(args ...string) (string, error) {
	cmd := exec.Command("podman", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("podman %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(string(out)), nil
}

// podmanExists reports whether an `exists` subcommand succeeds. Exit status 1 means absent; anything else is an error.
func podmanExists(args ...string) (bool, error) {
	cmd := exec.Command("podman", args...)
	err := cmd.Run()
	if err == nil {
		return true, nil
	}
	var xerr *exec.ExitError
	if errors.As(err, &xerr) && xerr.ExitCode() == 1 {
		return false, nil
	}
	return false, fmt.Errorf("podman %v: %w", args, err)
}
