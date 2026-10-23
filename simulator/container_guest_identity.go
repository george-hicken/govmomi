// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package simulator

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// guestIdentityOwned is the RUN.guestIdentityFiles value that enables the option.
const guestIdentityOwned = "owned"

// maxHostnameLength is the longest label a hostname may have.
const maxHostnameLength = 63

var (
	// errGuestIdentityHostNetwork is returned for a RUN.guestIdentityFiles VM that
	// joins the host's network namespace, which shares the host's hostname and
	// resolver.
	errGuestIdentityHostNetwork = errors.New("RUN.guestIdentityFiles cannot join the host network")
	// errGuestIdentityRuntime is returned when the docker CLI is not podman, since
	// --no-hostname and --no-hosts are podman flags.
	errGuestIdentityRuntime = errors.New("RUN.guestIdentityFiles needs podman")
	// errGuestIdentityPolicy is returned for a RUN.guestIdentityFiles value other
	// than "owned".
	errGuestIdentityPolicy = errors.New("invalid RUN.guestIdentityFiles")
)

// parseGuestIdentity validates a RUN.guestIdentityFiles value. Only "owned" is
// accepted, so a misspelt value fails at power-on rather than leaving the
// container's files runtime-managed.
func parseGuestIdentity(value string) error {
	if value != guestIdentityOwned {
		return fmt.Errorf("%w: %q is not %q", errGuestIdentityPolicy, value, guestIdentityOwned)
	}
	return nil
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
// container, or nil when the option is off. The runtime no longer binds
// /etc/hostname, /etc/hosts or /etc/resolv.conf, so the image's own files apply
// and the guest can replace them. The hostname is set from vmName, so early boot
// has a sane name before the guest changes it.
//
// sethostname needs CAP_SYS_ADMIN in the container's UTS namespace, so the flags
// add it, unless opts already grants it. That also widens the container's
// privileges beyond the hostname: the capability is not limited to the UTS namespace.
func guestIdentityCreateArgs(vmName string, opts createOptions) []string {
	if !opts.GuestIdentityFiles {
		return nil
	}
	args := []string{"--no-hostname", "--no-hosts", "--dns", "none", "--hostname", guestHostname(vmName)}
	if !opts.Privileged && !opts.NestedContainers && !opts.Sysctl && !slices.Contains(opts.CapAdd, "SYS_ADMIN") {
		args = append(args, "--cap-add", "SYS_ADMIN")
	}
	return args
}
