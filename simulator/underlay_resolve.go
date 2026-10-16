// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package simulator

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
)

var (
	// ErrPnicNotAttached is returned when a host pNIC carrying a vNIC's network has no
	// attachment in the topology, and is not the vmnic0 management default.
	ErrPnicNotAttached = errors.New("pnic not attached to an underlay")

	// ErrUplinksSpanUnderlays is returned when the uplinks of one network on one host land on different underlays.
	ErrUplinksSpanUnderlays = errors.New("uplinks span underlays")

	// ErrVlanModeNotSimulated is returned for trunk and PVLAN port configurations, which are not modelled yet.
	ErrVlanModeNotSimulated = errors.New("vlan mode not simulated")
)

// vlanTagRx matches the "v<N>" tag of a domain key, with N in 1..9999 (the range check is in parseDomain).
var vlanTagRx = regexp.MustCompile(`^v([1-9][0-9]{0,3})$`)

// Domain is one L2 broadcast domain: a VLAN or the untagged segment of an underlay, or an NSX segment.
// An NSX segment is independent of the underlay that carries it, so its key omits the underlay.
type Domain struct {
	Underlay string
	VLAN     int    // zero for untagged and NSX
	Segment  string // NSX segment id, empty otherwise
}

// Key is the isolation key: "<underlay>/v<N>", "<underlay>/untagged" or "seg:<id>".
// Two vNICs share a network only when their keys are equal.
func (d Domain) Key() string {
	if d.Segment != "" {
		return "seg:" + d.Segment
	}
	if d.VLAN == 0 {
		return d.Underlay + "/untagged"
	}
	return fmt.Sprintf("%s/v%d", d.Underlay, d.VLAN)
}

// networkName is the podman network for the domain. The run prefix keeps runs apart.
func (d Domain) networkName(run string) string {
	switch {
	case d.Segment != "":
		return run + "-seg-" + d.Segment
	case d.VLAN == 0:
		return run + "-" + d.Underlay + "-untagged"
	default:
		return fmt.Sprintf("%s-%s-v%d", run, d.Underlay, d.VLAN)
	}
}

// parseDomain parses a domain key as written in a router interface or by Key.
func parseDomain(key string) (Domain, error) {
	if seg, ok := strings.CutPrefix(key, "seg:"); ok {
		if !nameRx.MatchString(seg) {
			return Domain{}, fmt.Errorf("domain %q: segment id must match %s", key, nameRx)
		}
		return Domain{Segment: seg}, nil
	}

	underlay, tag, ok := strings.Cut(key, "/")
	if !ok || !nameRx.MatchString(underlay) {
		return Domain{}, fmt.Errorf("domain %q: want <underlay>/v<N>, <underlay>/untagged or seg:<id>", key)
	}
	if tag == "untagged" {
		return Domain{Underlay: underlay}, nil
	}

	m := vlanTagRx.FindStringSubmatch(tag)
	if m == nil {
		return Domain{}, fmt.Errorf("domain %q: want <underlay>/v<N>, <underlay>/untagged or seg:<id>", key)
	}
	vlan, _ := strconv.Atoi(m[1])
	if vlan > 4094 {
		return Domain{}, fmt.Errorf("domain %q: vlan %d out of range 1..4094", key, vlan)
	}

	return Domain{Underlay: underlay, VLAN: vlan}, nil
}

// pnicKeyPrefix is the prefix of a HostPhysicalNic key; the device name follows it.
const pnicKeyPrefix = "key-vim.host.PhysicalNic-"

// wiring is a vNIC's network as read from the inventory, reduced to what resolution needs.
type wiring struct {
	host    string
	uplinks []string // host pNIC device names carrying the network

	vlan    int // zero for untagged
	trunk   bool
	pvlan   bool
	segment string // NSX segment id, empty otherwise
}

// ResolveVNIC maps a vNIC's network on a host to its L2 domain through the topology's attachments.
// The host argument and the uplinks and VLAN mode in the inventory decide the domain; nothing else does.
func (t *Topology) ResolveVNIC(ctx *Context, host *HostSystem, network mo.Reference) (Domain, error) {
	w, err := wiringOf(ctx, host, network)
	if err != nil {
		return Domain{}, err
	}

	return t.resolve(w)
}

func (t *Topology) resolve(w wiring) (Domain, error) {
	if w.trunk || w.pvlan {
		return Domain{}, fmt.Errorf("%w: host %s", ErrVlanModeNotSimulated, w.host)
	}

	if len(w.uplinks) == 0 {
		return Domain{}, fmt.Errorf("%w: no uplink for network on host %s", ErrPnicNotAttached, w.host)
	}

	var underlay string
	for _, pnic := range w.uplinks {
		u, err := t.attachedUnderlay(w.host, pnic)
		if err != nil {
			return Domain{}, err
		}
		if underlay == "" {
			underlay = u
		} else if u != underlay {
			return Domain{}, fmt.Errorf("%w: host %s: %s on %s, %s on %s", ErrUplinksSpanUnderlays, w.host, w.uplinks[0], underlay, pnic, u)
		}
	}

	if w.segment != "" {
		return Domain{Underlay: underlay, Segment: w.segment}, nil
	}
	if w.vlan < 0 || w.vlan > 4094 {
		return Domain{}, fmt.Errorf("vlan %d out of range 0..4094", w.vlan)
	}

	return Domain{Underlay: underlay, VLAN: w.vlan}, nil
}

// attachedUnderlay returns the underlay a host pNIC is cabled to. An exact host attachment wins over
// a "*" one. vmnic0 with no attachment defaults to the management underlay, and any other unmapped pNIC fails.
func (t *Topology) attachedUnderlay(host, pnic string) (string, error) {
	for _, a := range t.Attachments {
		if a.Host == host && a.Pnic == pnic {
			return a.Underlay, nil
		}
	}
	for _, a := range t.Attachments {
		if a.Host == "*" && a.Pnic == pnic {
			return a.Underlay, nil
		}
	}

	if pnic == "vmnic0" {
		for _, u := range t.Underlays {
			if u.Management {
				return u.Name, nil
			}
		}
	}

	return "", fmt.Errorf("%w: %s on host %s", ErrPnicNotAttached, pnic, host)
}

// wiringOf reads a vNIC's network object and the host's switch tables into a wiring.
func wiringOf(ctx *Context, host *HostSystem, network mo.Reference) (wiring, error) {
	w := wiring{host: host.Name}
	if host.Config == nil || host.Config.Network == nil {
		return w, fmt.Errorf("%w: host %s has no network configuration", ErrPnicNotAttached, host.Name)
	}
	netInfo := host.Config.Network

	switch n := ctx.Map.Get(network.Reference()).(type) {
	case *DistributedVirtualPortgroup:
		dvsRef := n.Config.DistributedVirtualSwitch
		var dvsUUID string
		if dvsRef != nil {
			if dvs, ok := ctx.Map.Get(*dvsRef).(*VmwareDistributedVirtualSwitch); ok {
				dvsUUID = dvs.Uuid
			}
		}
		for _, ps := range netInfo.ProxySwitch {
			if dvsUUID != "" && ps.DvsUuid == dvsUUID {
				w.uplinks = pnicDevices(ps.Pnic)
			}
		}

		if n.Config.LogicalSwitchUuid != "" || n.Config.BackingType == "nsx" {
			w.segment = n.Config.SegmentId
			if w.segment == "" {
				w.segment = n.Config.LogicalSwitchUuid
			}
			return w, nil
		}

		if setting, ok := n.Config.DefaultPortConfig.(*types.VMwareDVSPortSetting); ok {
			switch vlan := setting.Vlan.(type) {
			case *types.VmwareDistributedVirtualSwitchVlanIdSpec:
				w.vlan = int(vlan.VlanId)
			case *types.VmwareDistributedVirtualSwitchTrunkVlanSpec:
				w.trunk = true
			case *types.VmwareDistributedVirtualSwitchPvlanSpec:
				w.pvlan = true
			}
		}

	case *mo.Network:
		for _, pg := range netInfo.Portgroup {
			if pg.Spec.Name != n.Name {
				continue
			}
			w.vlan = int(pg.Spec.VlanId)
			for _, vs := range netInfo.Vswitch {
				if vs.Name == pg.Spec.VswitchName {
					w.uplinks = pnicDevices(vs.Pnic)
				}
			}
		}

	case *mo.OpaqueNetwork:
		s, ok := n.Summary.(*types.OpaqueNetworkSummary)
		if !ok {
			return w, fmt.Errorf("network %s: unsupported summary %T", n.Name, n.Summary)
		}
		w.segment = s.OpaqueNetworkId
		for _, ps := range netInfo.ProxySwitch {
			w.uplinks = append(w.uplinks, pnicDevices(ps.Pnic)...)
		}

	default:
		return w, fmt.Errorf("network %s: unsupported type %T", network.Reference(), n)
	}

	return w, nil
}

// pnicDevices converts HostPhysicalNic keys to device names.
func pnicDevices(keys []string) []string {
	devs := make([]string, 0, len(keys))
	for _, k := range keys {
		devs = append(devs, strings.TrimPrefix(k, pnicKeyPrefix))
	}
	return devs
}
