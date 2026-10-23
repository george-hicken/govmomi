// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package simulator

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// The RUN.sysctl overlay. Host-global sysctls such as net.ipv4.tcp_mem are
// not namespaced, so a container cannot write them: the kernel answers ENOENT
// or EACCES. A guest that writes them, as a TuneD or an appliance agent does,
// then fails. RUN.sysctl lets such a write land in a file vcsim seeds with the
// host's value, so the guest's writes succeed and read back, and the host is
// unchanged. The emulated values are reported as SysctlEmulatedNotApplied.
//
// The container runs with /proc/sys unmasked and NET_ADMIN, so namespaced keys
// take real writes in the container's own network namespace; sysctlPreflight
// refuses the host's. The image's entrypoint is replaced by overlay.sh, which
// mounts the emulated values and then execs the image's own entrypoint and
// command. The mounts need CAP_SYS_ADMIN, which overlay.sh drops from the
// bounding set before exec unless the container is privileged or runs nested
// containers.

// SysctlEmulatedNotApplied is the status of an emulated sysctl. A write to it
// is not an effect on the host, so it is never reported as applied.
const SysctlEmulatedNotApplied = "emulated, not applied"

const (
	// sysctlOverlayDir is where vcsim mounts the overlay's directory, read-only,
	// in the container. It holds overlay.sh and the seed file.
	sysctlOverlayDir = "/usr/libexec/vcsim-sysctl"
	// sysctlOverlayEntrypoint is the overlay script in the container.
	sysctlOverlayEntrypoint = sysctlOverlayDir + "/overlay.sh"
	// sysctlSeedFileName is the seed file in the overlay's directory.
	sysctlSeedFileName = "seed"
	// sysctlProcPath is the host's and the container's /proc/sys.
	sysctlProcPath = "/proc/sys"
)

//go:embed container_sysctl_overlay.sh
var sysctlOverlayScript string

var (
	// errSysctlHostNetwork is returned for a RUN.sysctl VM that joins the host's
	// network namespace, where namespaced writes would reach the host.
	errSysctlHostNetwork = errors.New("RUN.sysctl needs a private network namespace")
	// errSysctlPolicy is returned for a RUN.sysctl value vcsim cannot apply.
	errSysctlPolicy = errors.New("invalid RUN.sysctl")

	sysctlKeyPattern = regexp.MustCompile(`^[a-z0-9_-]+(\.[a-z0-9_-]+)*$`)
)

// sysctlPolicy is a parsed RUN.sysctl value.
type sysctlPolicy struct {
	// emulated are the host-global keys whose writes are emulated, in value order.
	emulated []string
}

// sysctlValue is a sysctl key and its value.
type sysctlValue struct {
	Key   string
	Value string
}

// SysctlEvidence is one emulated sysctl of a container VM.
type SysctlEvidence struct {
	Key string
	// Seed is the host's value at power-on, which vcsim gave the container.
	Seed string
	// Current is the value the running container reads back.
	Current string
	// Status is SysctlEmulatedNotApplied.
	Status string
}

// parseSysctl parses a RUN.sysctl value, "emulate=<key>,<key>...". The keys are
// dotted sysctl names. An empty list unmasks /proc/sys without emulating any key.
func parseSysctl(value string) (sysctlPolicy, error) {
	var p sysctlPolicy
	list, ok := strings.CutPrefix(strings.TrimSpace(value), "emulate=")
	if !ok {
		return p, fmt.Errorf("%w: %q is not emulate=<keys>", errSysctlPolicy, value)
	}
	if list == "" {
		return p, nil
	}
	for _, key := range strings.Split(list, ",") {
		key = strings.TrimSpace(key)
		if !sysctlKeyPattern.MatchString(key) {
			return p, fmt.Errorf("%w: %q is not a sysctl key", errSysctlPolicy, key)
		}
		if !slices.Contains(p.emulated, key) {
			p.emulated = append(p.emulated, key)
		}
	}
	return p, nil
}

// sysctlPreflight refuses a RUN.sysctl VM that cannot be run safely. It is
// checked at power-on, before any host resource is created.
func sysctlPreflight(networks []string, jsonArgs bool) error {
	if !jsonArgs {
		return fmt.Errorf("%w: RUN.container must be a JSON array, so the overlay can run the image's entrypoint", errSysctlPolicy)
	}
	// The runtime's host network joins the container to the host's netns, where
	// the unmasked /proc/sys would write the host's namespaced keys.
	for _, n := range networks {
		if n == "host" {
			return fmt.Errorf("%w: RUN.network is %q", errSysctlHostNetwork, n)
		}
	}
	return nil
}

// sysctlSeedValues reads the host value of each key under root, a procfs sysctl
// directory. A key the host lacks seeds the empty value.
func sysctlSeedValues(root string, keys []string) ([]sysctlValue, error) {
	seeds := make([]sysctlValue, 0, len(keys))
	for _, key := range keys {
		b, err := os.ReadFile(filepath.Join(root, strings.ReplaceAll(key, ".", "/")))
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("reading host %s: %w", key, err)
		}
		value := strings.TrimSuffix(string(b), "\n")
		if strings.Contains(value, "\n") {
			return nil, fmt.Errorf("%w: host value of %s spans lines", errSysctlPolicy, key)
		}
		seeds = append(seeds, sysctlValue{Key: key, Value: value})
	}
	return seeds, nil
}

// writeSysctlOverlay writes the overlay script and its seed into dir, which
// is mounted at sysctlOverlayDir. The seed's first line says whether the
// script drops CAP_SYS_ADMIN before exec.
func writeSysctlOverlay(dir string, dropSysAdmin bool, seeds []sysctlValue) error {
	var seed strings.Builder
	fmt.Fprintf(&seed, "drop-sys-admin=%t\n", dropSysAdmin)
	for _, s := range seeds {
		fmt.Fprintf(&seed, "%s=%s\n", s.Key, s.Value)
	}
	if err := os.WriteFile(filepath.Join(dir, "overlay.sh"), []byte(sysctlOverlayScript), 0o555); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, sysctlSeedFileName), []byte(seed.String()), 0o444)
}

// sysctlCreateArgs returns the docker create flags for a container that runs
// the overlay: /proc/sys unmasked, the capabilities it needs, and the overlay
// as the entrypoint. NET_ADMIN lets the workload write namespaced net.* keys,
// which the kernel refuses without it even in a private netns. SYS_ADMIN is for
// the overlay's mounts. The overlay's directory is a volume the caller adds.
func sysctlCreateArgs() []string {
	return []string{
		"--security-opt", "unmask=" + sysctlProcPath,
		"--cap-add", "NET_ADMIN",
		"--cap-add", "SYS_ADMIN",
		"--entrypoint", sysctlOverlayEntrypoint,
	}
}

// imageArgv returns the command the image runs with the given command
// arguments: its entrypoint followed by args, or by its cmd when args is empty.
func imageArgv(image string, args []string) ([]string, error) {
	cmd := exec.Command("docker", "image", "inspect", image)
	out, err := cmd.Output()
	if err != nil {
		return nil, commandError("image inspect", err)
	}

	var inspected []struct {
		Config struct {
			Entrypoint []string
			Cmd        []string
		}
	}
	if err := json.Unmarshal(out, &inspected); err != nil {
		return nil, fmt.Errorf("image inspect %s: %w", image, err)
	}
	if len(inspected) == 0 {
		return nil, fmt.Errorf("image inspect %s: no image", image)
	}

	config := inspected[0].Config
	argv := slices.Clone(config.Entrypoint)
	if len(args) > 0 {
		argv = append(argv, args...)
	} else {
		argv = append(argv, config.Cmd...)
	}
	if len(argv) == 0 {
		return nil, fmt.Errorf("image %s has no entrypoint or command", image)
	}
	return argv, nil
}

// sysctlKeyPath is the /proc/sys file of a dotted sysctl key.
func sysctlKeyPath(key string) string {
	return sysctlProcPath + "/" + strings.ReplaceAll(key, ".", "/")
}

// EmulatedSysctls returns the seed and the current value of each emulated sysctl
// of the VM's container, reading the current value from the running container.
// It returns none for a VM without RUN.sysctl emulation.
func (vm *VirtualMachine) EmulatedSysctls(ctx *Context) ([]SysctlEvidence, error) {
	if vm.svm == nil || len(vm.svm.sysctl) == 0 {
		return nil, nil
	}
	if vm.svm.c == nil {
		return nil, errors.New("RUN.sysctl: no container for the VM")
	}

	out := make([]SysctlEvidence, 0, len(vm.svm.sysctl))
	for _, s := range vm.svm.sysctl {
		current, err := vm.svm.c.exec(ctx, []string{"cat", sysctlKeyPath(s.Key)})
		if err != nil {
			return nil, fmt.Errorf("RUN.sysctl: reading %s: %w", s.Key, err)
		}
		out = append(out, SysctlEvidence{
			Key:     s.Key,
			Seed:    s.Value,
			Current: current,
			Status:  SysctlEmulatedNotApplied,
		})
	}
	return out, nil
}

// prepareSysctl checks the RUN.sysctl VM, seeds the emulated keys from the host,
// and writes the overlay into a host directory that lives as long as the container.
// It returns the command the overlay execs, the image's entrypoint and command
// with args, which args[0] names the image for.
func (svm *simVM) prepareSysctl(policy sysctlPolicy, networks, args []string, jsonArgs, dropSysAdmin bool) ([]string, error) {
	if err := sysctlPreflight(networks, jsonArgs); err != nil {
		return nil, err
	}
	seeds, err := sysctlSeedValues(sysctlProcPath, policy.emulated)
	if err != nil {
		return nil, err
	}
	argv, err := imageArgv(args[0], args[1:])
	if err != nil {
		return nil, err
	}

	dir, err := os.MkdirTemp("", "vcsim-sysctl-")
	if err != nil {
		return nil, fmt.Errorf("RUN.sysctl: %w", err)
	}
	if err := writeSysctlOverlay(dir, dropSysAdmin, seeds); err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("RUN.sysctl: %w", err)
	}
	svm.sysctlDir = dir
	svm.sysctl = seeds
	return argv, nil
}

// removeSysctl removes the overlay directory and forgets the seeds.
func (svm *simVM) removeSysctl() {
	if svm.sysctlDir != "" {
		_ = os.RemoveAll(svm.sysctlDir)
		svm.sysctlDir = ""
	}
	svm.sysctl = nil
}
