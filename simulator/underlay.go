// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package simulator

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/netip"
	"os"
	"regexp"

	"gopkg.in/yaml.v3"
)

// nameRx constrains run, underlay, router and segment names. They become podman object names and label values.
var nameRx = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,31}$`)

// Topology is the physical layer beneath a simulated vSphere inventory: the L2 underlays,
// the routers that join their domains, and which host pNICs plug into which underlay.
// The virtual layer (DVS, portgroups, VLAN IDs, NSX segments) stays in the inventory and
// is resolved onto this layer by ResolveVNIC.
type Topology struct {
	// Run prefixes and labels every podman object created for this topology.
	Run string `json:"run"`

	Underlays []Underlay `json:"underlays"`

	Routers []Router `json:"routers,omitempty"`

	// Attachments map a host pNIC to the underlay it is cabled to.
	Attachments []Attachment `json:"attachments,omitempty"`
}

// Underlay is an L2 medium. Each VLAN, or the untagged domain, on an underlay is a separate podman network.
type Underlay struct {
	Name string `json:"name"`

	// MTU is the podman network MTU of every domain on this underlay. Zero keeps the podman default.
	MTU int `json:"mtu,omitempty"`

	// Management marks the underlay that vmnic0 attaches to when no attachment maps it.
	Management bool `json:"management,omitempty"`
}

// Router is a container with forwarding enabled (or not) that holds one interface per domain it joins.
type Router struct {
	Name string `json:"name"`

	// Forward enables IPv4 forwarding in the router's namespace. Without it a router
	// attached to two domains does not bridge them.
	Forward bool `json:"forward"`

	Interfaces []RouterInterface `json:"interfaces"`

	Routes []Route `json:"routes,omitempty"`
}

// RouterInterface is one router link into a domain.
type RouterInterface struct {
	// Domain is "<underlay>/v<N>", "<underlay>/untagged" or "seg:<id>".
	Domain string `json:"domain"`

	// Address is the interface address in CIDR form.
	Address string `json:"address"`

	// MTU overrides the domain MTU on this link. Zero keeps the domain MTU.
	MTU int `json:"mtu,omitempty"`
}

// Route is a static route in a router's namespace.
type Route struct {
	// Dest is the destination prefix in CIDR form.
	Dest string `json:"dest"`

	// Via is the next-hop address, which must be on one of the router's domains.
	Via string `json:"via"`
}

// Attachment cables a host pNIC to an underlay. Host "*" matches every host, and an
// exact host match takes precedence over it.
type Attachment struct {
	Host     string `json:"host"`
	Pnic     string `json:"pnic"`
	Underlay string `json:"underlay"`
}

// LoadTopology reads a topology from a YAML or JSON file and validates it.
func LoadTopology(path string) (*Topology, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	t, err := ParseTopology(data)
	if err != nil {
		return nil, fmt.Errorf("topology %s: %w", path, err)
	}

	return t, nil
}

// ParseTopology decodes a topology from YAML or JSON (JSON is a YAML subset) and validates it.
// Unknown fields are rejected, so a misspelt key cannot silently keep a default.
func ParseTopology(data []byte) (*Topology, error) {
	var generic any
	if err := yaml.Unmarshal(data, &generic); err != nil {
		return nil, err
	}

	js, err := json.Marshal(generic)
	if err != nil {
		return nil, err
	}

	dec := json.NewDecoder(bytes.NewReader(js))
	dec.DisallowUnknownFields()

	var t Topology
	if err := dec.Decode(&t); err != nil {
		return nil, err
	}

	if err := t.Validate(); err != nil {
		return nil, err
	}

	return &t, nil
}

// Validate checks names, references and address syntax. It does not consult any host, so a
// topology that validates can still fail to resolve a particular vNIC.
func (t *Topology) Validate() error {
	if !nameRx.MatchString(t.Run) {
		return fmt.Errorf("run %q: must match %s", t.Run, nameRx)
	}

	underlays := make(map[string]bool, len(t.Underlays))
	management := 0
	for _, u := range t.Underlays {
		if !nameRx.MatchString(u.Name) {
			return fmt.Errorf("underlay %q: must match %s", u.Name, nameRx)
		}
		if underlays[u.Name] {
			return fmt.Errorf("underlay %q: defined more than once", u.Name)
		}
		if err := checkMTU(u.MTU); err != nil {
			return fmt.Errorf("underlay %q: %w", u.Name, err)
		}
		underlays[u.Name] = true
		if u.Management {
			management++
		}
	}
	if management > 1 {
		return fmt.Errorf("%d underlays marked management, at most one allowed", management)
	}

	routers := make(map[string]bool, len(t.Routers))
	for _, r := range t.Routers {
		if !nameRx.MatchString(r.Name) {
			return fmt.Errorf("router %q: must match %s", r.Name, nameRx)
		}
		if routers[r.Name] {
			return fmt.Errorf("router %q: defined more than once", r.Name)
		}
		routers[r.Name] = true

		if len(r.Interfaces) == 0 {
			return fmt.Errorf("router %q: no interfaces", r.Name)
		}
		for _, ifc := range r.Interfaces {
			d, err := parseDomain(ifc.Domain)
			if err != nil {
				return fmt.Errorf("router %q: %w", r.Name, err)
			}
			if d.Segment == "" && !underlays[d.Underlay] {
				return fmt.Errorf("router %q: domain %q: unknown underlay", r.Name, ifc.Domain)
			}
			if _, err := netip.ParsePrefix(ifc.Address); err != nil {
				return fmt.Errorf("router %q: address %q: %w", r.Name, ifc.Address, err)
			}
			if err := checkMTU(ifc.MTU); err != nil {
				return fmt.Errorf("router %q: %w", r.Name, err)
			}
		}
		for _, rt := range r.Routes {
			if _, err := netip.ParsePrefix(rt.Dest); err != nil {
				return fmt.Errorf("router %q: route dest %q: %w", r.Name, rt.Dest, err)
			}
			if _, err := netip.ParseAddr(rt.Via); err != nil {
				return fmt.Errorf("router %q: route via %q: %w", r.Name, rt.Via, err)
			}
		}
	}

	attached := make(map[string]bool, len(t.Attachments))
	for _, a := range t.Attachments {
		if a.Host == "" || a.Pnic == "" {
			return fmt.Errorf("attachment %+v: host and pnic are required", a)
		}
		if !underlays[a.Underlay] {
			return fmt.Errorf("attachment %s/%s: unknown underlay %q", a.Host, a.Pnic, a.Underlay)
		}
		key := a.Host + "/" + a.Pnic
		if attached[key] {
			return fmt.Errorf("attachment %s: defined more than once", key)
		}
		attached[key] = true
	}

	return nil
}

// checkMTU accepts zero (unset) or a jumbo-frame-bounded MTU.
func checkMTU(mtu int) error {
	if mtu == 0 || (mtu >= 576 && mtu <= 9216) {
		return nil
	}
	return fmt.Errorf("mtu %d: must be 0 or within 576..9216", mtu)
}
