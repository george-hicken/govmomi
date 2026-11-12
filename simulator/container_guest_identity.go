// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package simulator

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"
)

// guestIdentityAll is the RUN.guestIdentityFiles value that owns every file.
const guestIdentityAll = "owned"

// maxHostnameLength is the longest label a hostname may have.
const maxHostnameLength = 63

// guestIdentitySet is the set of files a container owns instead of the runtime's
// bind mounts. The zero value owns none, so the container is created as before.
type guestIdentitySet struct {
	hosts    bool // /etc/hosts
	hostname bool // /etc/hostname, and the container's hostname, which sethostname sets
	resolv   bool // /etc/resolv.conf, with no DNS server
}

// enabled reports whether the container owns any file.
func (s guestIdentitySet) enabled() bool {
	return s.hosts || s.hostname || s.resolv
}

var (
	// errGuestIdentityHostNetwork is returned for a RUN.guestIdentityFiles VM that
	// joins the host's network namespace, which shares the host's hostname and
	// resolver.
	errGuestIdentityHostNetwork = errors.New("RUN.guestIdentityFiles cannot join the host network")
	// errGuestIdentityRuntime is returned when the docker CLI is not podman, since
	// --no-hostname and --no-hosts are podman flags.
	errGuestIdentityRuntime = errors.New("RUN.guestIdentityFiles needs podman")
	// errGuestIdentityPolicy is returned for a RUN.guestIdentityFiles value that is
	// neither "owned" nor a list of the files "hosts", "hostname" and "resolv".
	errGuestIdentityPolicy = errors.New("invalid RUN.guestIdentityFiles")
)

// parseGuestIdentity validates a RUN.guestIdentityFiles value: "owned", which owns
// every file, or a comma-separated list of "hosts", "hostname" and "resolv", each
// at most once. Any other value fails at power-on, so a misspelt name cannot leave
// a file runtime-managed without notice.
func parseGuestIdentity(value string) (guestIdentitySet, error) {
	if value == guestIdentityAll {
		return guestIdentitySet{hosts: true, hostname: true, resolv: true}, nil
	}

	var set guestIdentitySet
	flags := map[string]*bool{"hosts": &set.hosts, "hostname": &set.hostname, "resolv": &set.resolv}
	for _, name := range strings.Split(value, ",") {
		flag, ok := flags[name]
		if !ok {
			return guestIdentitySet{}, fmt.Errorf(`%w: %q in %q, want %q or a list of "hosts", "hostname", "resolv"`,
				errGuestIdentityPolicy, name, value, guestIdentityAll)
		}
		if *flag {
			return guestIdentitySet{}, fmt.Errorf("%w: %q listed twice in %q", errGuestIdentityPolicy, name, value)
		}
		*flag = true
	}
	return set, nil
}

// guestIdentityPreflight refuses a RUN.guestIdentityFiles VM that cannot own its
// files. It is checked at power-on, before any host resource is created.
func guestIdentityPreflight(networks []string) error {
	for _, n := range networks {
		if n == "host" {
			return fmt.Errorf("%w: RUN.network is %q", errGuestIdentityHostNetwork, n)
		}
	}
	if !runtimeNetwork().podman {
		return errGuestIdentityRuntime
	}
	return nil
}

// guestHostname returns a valid hostname for a VM name: lowercase letters, digits
// and hyphens, no leading or trailing hyphen, at most maxHostnameLength bytes.
// Any run of other characters becomes one hyphen. A name with no letters or
// digits left becomes "vm".
func guestHostname(name string) string {
	var b strings.Builder
	pendingHyphen := false
	for _, r := range strings.ToLower(name) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			if pendingHyphen && b.Len() > 0 {
				b.WriteByte('-')
			}
			pendingHyphen = false
			b.WriteRune(r)
			continue
		}
		pendingHyphen = true
	}
	s := b.String()
	if len(s) > maxHostnameLength {
		s = strings.TrimRight(s[:maxHostnameLength], "-")
	}
	if s == "" {
		return "vm"
	}
	return s
}

// guestIdentityCreateArgs returns the docker create flags for a RUN.guestIdentityFiles
// container, or nil when no file is owned. Each owned file drops its runtime bind
// mount, so the image's own file applies and the guest can replace it: hosts by
// --no-hosts, hostname by --no-hostname, resolv by --dns none. Owning the hostname
// also sets it from vmName, so early boot has a sane name before the guest changes it.
//
// sethostname needs CAP_SYS_ADMIN in the container's UTS namespace, so the flags
// add it when the hostname is owned, unless opts already grants it. That also
// widens the container's privileges beyond the hostname: the capability is not
// limited to the UTS namespace.
func guestIdentityCreateArgs(vmName string, opts createOptions) []string {
	set := opts.GuestIdentity
	if !set.enabled() {
		return nil
	}
	var args []string
	if set.hostname {
		args = append(args, "--no-hostname")
	}
	if set.hosts {
		args = append(args, "--no-hosts")
	}
	if set.resolv {
		args = append(args, "--dns", "none")
	}
	if set.hostname {
		args = append(args, "--hostname", guestHostname(vmName))
		if !opts.Privileged && !opts.NestedContainers && !opts.Sysctl && !slices.Contains(opts.CapAdd, "SYS_ADMIN") {
			args = append(args, "--cap-add", "SYS_ADMIN")
		}
	}
	return args
}

// guestIdentitySeed is a file the guest owns, at its tar path relative to the
// container's root, with the content it is seeded with when the image lacks it.
type guestIdentitySeed struct {
	path    string
	content string
}

// guestIdentitySeeds returns the owned files with the content podman writes for
// them without --no-hosts and --no-hostname: the loopback names and the hostname.
// The container has no network entry of its own, so no address line is written.
func guestIdentitySeeds(vmName string, set guestIdentitySet) []guestIdentitySeed {
	host := guestHostname(vmName)
	var seeds []guestIdentitySeed
	if set.hosts {
		seeds = append(seeds, guestIdentitySeed{
			path:    "etc/hosts",
			content: "::1\tipv6-localhost ipv6-loopback\n127.0.0.1\tlocalhost\n127.0.0.1\t" + host + "\n",
		})
	}
	if set.hostname {
		seeds = append(seeds, guestIdentitySeed{path: "etc/hostname", content: host + "\n"})
	}
	return seeds
}

// missingGuestIdentitySeeds returns the seeds whose path the image does not have.
// A file the image ships is kept as the image has it.
func missingGuestIdentitySeeds(seeds []guestIdentitySeed, present map[string]bool) []guestIdentitySeed {
	var missing []guestIdentitySeed
	for _, s := range seeds {
		if !present[s.path] {
			missing = append(missing, s)
		}
	}
	return missing
}

// seedGuestIdentity writes the owned files that the image lacks into a created
// container, before it starts, so the guest has a usable /etc/hosts and
// /etc/hostname: the runtime no longer writes them, and an image may not ship them.
func seedGuestIdentity(id, vmName string, set guestIdentitySet) error {
	seeds := guestIdentitySeeds(vmName, set)
	if len(seeds) == 0 {
		return nil
	}
	present, err := containerEtcNames(id)
	if err != nil {
		return err
	}
	missing := missingGuestIdentitySeeds(seeds, present)
	if len(missing) == 0 {
		return nil
	}
	return copyGuestFiles(id, missing)
}

// containerEtcNames returns the names under the container's /etc as tar paths
// relative to /. docker exec cannot run in a created container, so the names are
// read with docker cp.
func containerEtcNames(id string) (map[string]bool, error) {
	cmd := exec.Command("docker", "cp", id+":/etc", "-")
	cmd.Stderr = os.Stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("container cp %s: %w", id, err)
	}
	names, readErr := tarNames(stdout)
	waitErr := cmd.Wait()
	if readErr != nil || waitErr != nil {
		return nil, fmt.Errorf("container cp %s: read {%v}, wait {%v}", id, readErr, waitErr)
	}
	return names, nil
}

// tarNames returns the entry names of a tar stream, without a trailing slash.
func tarNames(r io.Reader) (map[string]bool, error) {
	names := map[string]bool{}
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return names, nil
		}
		if err != nil {
			return nil, err
		}
		names[strings.TrimSuffix(h.Name, "/")] = true
	}
}

// copyGuestFiles writes the files into a created container's root as root-owned,
// mode 0644 regular files.
func copyGuestFiles(id string, files []guestIdentitySeed) error {
	cmd := exec.Command("docker", "cp", "-", id+":/")
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("container cp %s: %w", id, err)
	}

	tw := tar.NewWriter(stdin)
	var writeErr error
	for _, f := range files {
		h := &tar.Header{Name: f.path, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(f.content)), ModTime: time.Now()}
		if writeErr = tw.WriteHeader(h); writeErr != nil {
			break
		}
		if _, writeErr = io.WriteString(tw, f.content); writeErr != nil {
			break
		}
	}
	twErr := tw.Close()
	stdinErr := stdin.Close()
	waitErr := cmd.Wait()
	if writeErr != nil || twErr != nil || stdinErr != nil || waitErr != nil {
		return fmt.Errorf("container cp %s: write {%v}, tw {%v}, stdin {%v}, wait {%v}",
			id, writeErr, twErr, stdinErr, waitErr)
	}
	return nil
}
