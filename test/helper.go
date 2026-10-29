// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package test

import (
	"context"
	"fmt"
	"log"
	"math/rand/v2"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vmware/govmomi/vim25"
	"github.com/vmware/govmomi/vim25/methods"
	"github.com/vmware/govmomi/vim25/soap"
	"github.com/vmware/govmomi/vim25/types"
)

// ContainerTestsEnv, set to 1, opts in to tests that start docker or podman containers.
const ContainerTestsEnv = "VCSIM_CONTAINER_TESTS"

// RequireContainerOptIn skips t unless ContainerTestsEnv=1, so a plain go test never starts
// containers on a host whose container runtime other work is using.
func RequireContainerOptIn(t testing.TB) {
	t.Helper()
	if os.Getenv(ContainerTestsEnv) != "1" {
		t.Skipf("container test: set %s=1 to run", ContainerTestsEnv)
	}
}

func HasDocker() bool {
	if runtime.GOOS != "linux" {
		return false
	}
	if _, err := exec.LookPath("docker"); err != nil {
		return false
	}
	return true
}

var (
	containerBridgeOnce sync.Once
	containerBridgeOK   bool
)

// HasContainerBridge reports whether containers on this host can join a bridge
// network. Under podman, netavark programs the bridge firewall with nftables
// expressions. On a kernel without nft_fib, container start fails with
//
//	netavark: nftables error: "nft" did not return successfully while applying ruleset
//
// as seen with netavark 2.0.0 on Photon 5 kernel 6.12.112-5.ph5-esx. NETAVARK_FW=none
// turns that programming off. Docker does not use netavark, so it is not probed.
func HasContainerBridge() bool {
	containerBridgeOnce.Do(func() {
		containerBridgeOK = !isPodman() || os.Getenv("NETAVARK_FW") == "none" || probeContainerBridge()
	})
	return containerBridgeOK
}

const (
	containerBridgeProbeImage   = "docker.io/library/alpine:3.20.3"
	containerBridgeProbeTimeout = 60 * time.Second
)

// probeContainerBridge starts a container on a throwaway bridge network. It
// returns false only for the netavark nftables error. Any other failure, and a
// timeout (which includes the image pull), returns true so the test reports it.
func probeContainerBridge() bool {
	ctx, cancel := context.WithTimeout(context.Background(), containerBridgeProbeTimeout)
	defer cancel()

	name := fmt.Sprintf("govmomi-bridge-probe-%08x", rand.Uint32())
	if out, err := exec.CommandContext(ctx, "docker", "network", "create", name).CombinedOutput(); err != nil {
		log.Printf("container bridge probe: network create %s: %v: %s", name, err, out)
		return true
	}
	defer func() {
		// Cleanup gets its own deadline, since ctx may already have expired.
		cctx, ccancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer ccancel()
		exec.CommandContext(cctx, "docker", "rm", "-f", name).Run()
		if err := exec.CommandContext(cctx, "docker", "network", "rm", name).Run(); err != nil {
			log.Printf("container bridge probe: network rm %s: %v", name, err)
		}
	}()

	out, err := exec.CommandContext(ctx, "docker", "run", "--name", name, "--network", name, containerBridgeProbeImage, "true").CombinedOutput()
	if ctx.Err() != nil {
		log.Printf("container bridge probe: timed out after %s", containerBridgeProbeTimeout)
		return true
	}
	return err == nil || !(strings.Contains(string(out), "netavark") && strings.Contains(string(out), "nftables"))
}

// isPodman reports whether the docker CLI is podman. It probes docker info with
// a podman-only template field, since the docker --version string is not
// reliable: some podman docker shims print "docker version".
func isPodman() bool {
	return exec.Command("docker", "info", "--format", "{{.Host.Security.Rootless}}").Run() == nil
}

var (
	isRootlessPodmanOnce   sync.Once
	isRootlessPodmanResult bool
)

// IsRootlessPodman returns true when the docker CLI is backed by rootless podman.
// Rootless podman has restrictions such as the inability to bind-mount system
// paths like /sys/class/dmi/id.
//
// The detection result is memoized (sync.Once): the underlying runtime does
// not change within a test binary's lifetime, and each call otherwise spawns
// up to two subprocesses, which adds up across the many container-backed
// tests that call this indirectly via ApplyContainerRuntimeDefaults.
func IsRootlessPodman() bool {
	isRootlessPodmanOnce.Do(func() {
		isRootlessPodmanResult = detectRootlessPodman()
	})
	return isRootlessPodmanResult
}

func detectRootlessPodman() bool {
	out, err := exec.Command("docker", "--version").Output()
	if err != nil {
		log.Printf("test: docker --version: %v", err)
		return false
	}
	if !strings.Contains(string(out), "podman") {
		return false
	}
	out, err = exec.Command("podman", "info", "--format", "{{.Host.Security.Rootless}}").Output()
	if err != nil {
		log.Printf("test: docker CLI reports podman, but podman info failed (rootless-podman detection assuming false): %v", err)
		return false
	}
	return strings.TrimSpace(string(out)) == "true"
}

// containerRuntimeDefaults returns the ExtraConfig defaults appropriate for
// the current container runtime and spec.  Returns nil, nil when no
// runtime-specific defaults are needed.
//
// For rootless podman:
//   - RUN.mountdmi=false is always added (rootless cannot bind-mount
//     /sys/class/dmi/id).
//   - RUN.network is added when the spec uses container backing; the bridge
//     is created on demand unless VCSIM_NETWORK is set (in which case the
//     caller-supplied network is used as-is).
func containerRuntimeDefaults(spec *types.VirtualMachineConfigSpec) ([]types.BaseOptionValue, error) {
	if !IsRootlessPodman() {
		return nil, nil
	}

	defaults := []types.BaseOptionValue{
		&types.OptionValue{Key: "RUN.mountdmi", Value: "false"},
	}

	if hasContainerBacking(spec) {
		network := os.Getenv("VCSIM_NETWORK")
		if network == "" {
			network = "vcsim-test-bridge"
			if err := ensureContainerBridge(network); err != nil {
				return nil, fmt.Errorf("creating default container bridge %q: %w", network, err)
			}
		}
		defaults = append(defaults, &types.OptionValue{Key: "RUN.network", Value: network})
	}

	return defaults, nil
}

// hasContainerBacking reports whether spec contains a RUN.container ExtraConfig key.
func hasContainerBacking(spec *types.VirtualMachineConfigSpec) bool {
	for _, opt := range spec.ExtraConfig {
		if opt.GetOptionValue().Key == "RUN.container" {
			return true
		}
	}
	return false
}

// ensureContainerBridge creates the named Docker/Podman bridge network if it
// does not already exist.
func ensureContainerBridge(name string) error {
	out, err := exec.Command("docker", "network", "inspect", name).Output()
	if err == nil && len(out) > 2 { // non-empty JSON array means it exists
		return nil
	}
	return exec.Command("docker", "network", "create", name).Run()
}

// ContainerNetworkFromSpec returns the RUN.network value from spec's
// ExtraConfig, or "" when not set.  After ApplyContainerRuntimeDefaults, this
// yields the bridge name that was configured for rootless podman, or "" on
// runtimes that use the default bridge (e.g. root Docker).  Pass the result
// to any probe container that must reach the VM container by IP.
func ContainerNetworkFromSpec(spec *types.VirtualMachineConfigSpec) string {
	for _, opt := range spec.ExtraConfig {
		ov := opt.GetOptionValue()
		if ov.Key == "RUN.network" {
			return fmt.Sprint(ov.Value)
		}
	}
	return ""
}

// ContainerCurlProbeArgs returns the "docker run" argument list for probing
// targetURL from a curlimages/curl container, joining network (if non-empty,
// typically the result of ContainerNetworkFromSpec) so the probe can reach a
// container-backed VM that is not on the runtime's default bridge (e.g.
// rootless podman).
func ContainerCurlProbeArgs(network, targetURL string) []string {
	args := []string{"run", "--rm"}
	if network != "" {
		args = append(args, "--network", network)
	}
	return append(args, "curlimages/curl", "curl", "-f", targetURL)
}

// ApplyContainerRuntimeDefaults adds runtime-appropriate ExtraConfig defaults
// to spec.  It is a no-op when the current runtime requires no special
// treatment.
//
// An error is returned when the spec already contains an explicit value for a
// key that the runtime default would change.  An exact value match is treated
// as a silent no-op: the caller already expressed the same intent.  A
// differing value is an error because silently overriding explicit test intent
// would mask problems.
func ApplyContainerRuntimeDefaults(spec *types.VirtualMachineConfigSpec) error {
	defaults, err := containerRuntimeDefaults(spec)
	if err != nil {
		return err
	}
	if len(defaults) == 0 {
		return nil
	}

	for _, def := range defaults {
		dov := def.GetOptionValue()
		defaultVal := fmt.Sprint(dov.Value)

		found := false
		for _, existing := range spec.ExtraConfig {
			eov := existing.GetOptionValue()
			if eov.Key != dov.Key {
				continue
			}
			found = true
			if existingVal := fmt.Sprint(eov.Value); existingVal != defaultVal {
				return fmt.Errorf(
					"container runtime default %q=%q conflicts with explicit ExtraConfig value %q; "+
						"remove the explicit setting to use runtime defaults, or handle the "+
						"runtime difference in the test itself",
					dov.Key, defaultVal, existingVal,
				)
			}
			// Already set to the correct value; nothing to add.
			break
		}

		if !found {
			spec.ExtraConfig = append(spec.ExtraConfig, def)
		}
	}
	return nil
}

// URL parses the GOVMOMI_TEST_URL environment variable if set.
func URL() *url.URL {
	s := os.Getenv("GOVMOMI_TEST_URL")
	if s == "" {
		return nil
	}
	u, err := soap.ParseURL(s)
	if err != nil {
		panic(err)
	}
	return u
}

// NewAuthenticatedClient creates a new vim25.Client, authenticates the user
// specified in the test URL, and returns it.
func NewAuthenticatedClient(t *testing.T) *vim25.Client {
	u := URL()
	if u == nil {
		t.SkipNow()
	}

	soapClient := soap.NewClient(u, true)
	vimClient, err := vim25.NewClient(context.Background(), soapClient)
	if err != nil {
		t.Fatal(err)
	}

	req := types.Login{
		This: *vimClient.ServiceContent.SessionManager,
	}

	req.UserName = u.User.Username()
	if pw, ok := u.User.Password(); ok {
		req.Password = pw
	}

	_, err = methods.Login(context.Background(), vimClient, &req)
	if err != nil {
		t.Fatal(err)
	}

	return vimClient
}
