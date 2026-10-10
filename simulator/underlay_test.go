// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package simulator

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
)

const testTopologyYAML = `
run: sv2-test
underlays:
  - name: mgmt
    management: true
  - name: wl
    mtu: 1400
  - name: transit
routers:
  - name: r1
    forward: true
    interfaces:
      - domain: wl/v10
        address: 10.10.0.1/24
      - domain: transit/untagged
        address: 10.255.0.1/30
        mtu: 1300
    routes:
      - dest: 10.20.0.0/24
        via: 10.255.0.2
attachments:
  - host: "*"
    pnic: vmnic1
    underlay: wl
`

const testTopologyJSON = `{
  "run": "sv2-test",
  "underlays": [
    {"name": "mgmt", "management": true},
    {"name": "wl", "mtu": 1400},
    {"name": "transit"}
  ],
  "routers": [
    {"name": "r1", "forward": true,
     "interfaces": [
       {"domain": "wl/v10", "address": "10.10.0.1/24"},
       {"domain": "transit/untagged", "address": "10.255.0.1/30", "mtu": 1300}
     ],
     "routes": [{"dest": "10.20.0.0/24", "via": "10.255.0.2"}]}
  ],
  "attachments": [{"host": "*", "pnic": "vmnic1", "underlay": "wl"}]
}`

func TestParseTopology(t *testing.T) {
	fromYAML, err := ParseTopology([]byte(testTopologyYAML))
	if err != nil {
		t.Fatal(err)
	}
	fromJSON, err := ParseTopology([]byte(testTopologyJSON))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fromYAML, fromJSON) {
		t.Errorf("YAML and JSON decode differently:\n%+v\n%+v", fromYAML, fromJSON)
	}
	if got := fromYAML.Routers[0].Interfaces[1].MTU; got != 1300 {
		t.Errorf("router interface mtu: got %d, want 1300", got)
	}
}

func TestParseTopologyErrors(t *testing.T) {
	tests := []struct {
		name string
		doc  string
		want string
	}{
		{"unknown field", "run: a\nunderlay: []\n", "unknown field"},
		{"bad run", "run: 'bad run'\n", "run"},
		{"empty", "{}", "run"},
		{"duplicate underlay", "run: a\nunderlays: [{name: x}, {name: x}]\n", "more than once"},
		{"two management", "run: a\nunderlays: [{name: x, management: true}, {name: y, management: true}]\n", "management"},
		{"mtu too small", "run: a\nunderlays: [{name: x, mtu: 100}]\n", "mtu"},
		{"router unknown underlay", "run: a\nunderlays: [{name: x}]\nrouters: [{name: r, interfaces: [{domain: y/v10, address: 10.0.0.1/24}]}]\n", "unknown underlay"},
		{"router vlan out of range", "run: a\nunderlays: [{name: x}]\nrouters: [{name: r, interfaces: [{domain: x/v4095, address: 10.0.0.1/24}]}]\n", "range"},
		{"router bad address", "run: a\nunderlays: [{name: x}]\nrouters: [{name: r, interfaces: [{domain: x/untagged, address: 10.0.0.1}]}]\n", "address"},
		{"router no interfaces", "run: a\nrouters: [{name: r}]\n", "no interfaces"},
		{"bad route", "run: a\nunderlays: [{name: x}]\nrouters: [{name: r, interfaces: [{domain: x/untagged, address: 10.0.0.1/24}], routes: [{dest: nope, via: 10.0.0.2}]}]\n", "route dest"},
		{"attachment unknown underlay", "run: a\nunderlays: [{name: x}]\nattachments: [{host: '*', pnic: vmnic1, underlay: y}]\n", "unknown underlay"},
		{"attachment duplicate", "run: a\nunderlays: [{name: x}]\nattachments: [{host: h, pnic: vmnic1, underlay: x}, {host: h, pnic: vmnic1, underlay: x}]\n", "more than once"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseTopology([]byte(tc.doc))
			if err == nil {
				t.Fatalf("want error containing %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not contain %q", err, tc.want)
			}
		})
	}
}

func TestDomainKeys(t *testing.T) {
	const run = "sv2-x"
	tests := []struct {
		in      string
		key     string
		netName string
	}{
		{"wl/v10", "wl/v10", "sv2-x-wl-v10"},
		{"wl/v4094", "wl/v4094", "sv2-x-wl-v4094"},
		{"mgmt/untagged", "mgmt/untagged", "sv2-x-mgmt-untagged"},
		{"seg:abc-123", "seg:abc-123", "sv2-x-seg-abc-123"},
	}
	for _, tc := range tests {
		d, err := parseDomain(tc.in)
		if err != nil {
			t.Fatalf("%s: %v", tc.in, err)
		}
		if got := d.Key(); got != tc.key {
			t.Errorf("%s: key %q, want %q", tc.in, got, tc.key)
		}
		if got := d.networkName(run); got != tc.netName {
			t.Errorf("%s: network %q, want %q", tc.in, got, tc.netName)
		}
	}

	for _, bad := range []string{"wl/v0", "wl/v4095", "wl/v010", "wl/v+10", "wl/v10x", "wl", "/v10", "seg:", "seg:a b"} {
		if _, err := parseDomain(bad); err == nil {
			t.Errorf("%q: want error", bad)
		}
	}
}

// resolveTopology has a management underlay, a workload underlay with a host-specific and a wildcard attachment,
// and a transit underlay reachable only by wildcard.
func resolveTopology(t *testing.T) *Topology {
	t.Helper()
	topo, err := ParseTopology([]byte(`
run: r
underlays:
  - {name: mgmt, management: true}
  - {name: wl}
  - {name: transit}
attachments:
  - {host: h1, pnic: vmnic1, underlay: wl}
  - {host: h1, pnic: vmnic2, underlay: mgmt}
  - {host: "*", pnic: vmnic2, underlay: transit}
  - {host: h2, pnic: vmnic1, underlay: wl}
  - {host: h2, pnic: vmnic3, underlay: wl}
`))
	if err != nil {
		t.Fatal(err)
	}
	return topo
}

// errAny marks a resolve case that must fail with an error that has no named sentinel.
var errAny = errors.New("any error")

func TestResolve(t *testing.T) {
	topo := resolveTopology(t)

	tests := []struct {
		name string
		w    wiring
		key  string
		err  error // errAny accepts any error
	}{
		{"vlan on attached pnic", wiring{host: "h1", uplinks: []string{"vmnic1"}, vlan: 10}, "wl/v10", nil},
		{"untagged on attached pnic", wiring{host: "h1", uplinks: []string{"vmnic1"}}, "wl/untagged", nil},
		{"host attachment beats wildcard", wiring{host: "h1", uplinks: []string{"vmnic2"}, vlan: 7}, "mgmt/v7", nil},
		{"wildcard attachment", wiring{host: "h9", uplinks: []string{"vmnic2"}, vlan: 7}, "transit/v7", nil},
		{"vmnic0 defaults to management", wiring{host: "h9", uplinks: []string{"vmnic0"}}, "mgmt/untagged", nil},
		{"vmnic0 default on a host with attachments", wiring{host: "h1", uplinks: []string{"vmnic0"}, vlan: 3}, "mgmt/v3", nil},
		{"unmapped vmnic1 on other host", wiring{host: "h3", uplinks: []string{"vmnic1"}}, "", ErrPnicNotAttached},
		{"unmapped vmnic4", wiring{host: "h1", uplinks: []string{"vmnic4"}}, "", ErrPnicNotAttached},
		{"no uplinks", wiring{host: "h1"}, "", ErrPnicNotAttached},
		{"two pnics on one underlay (LAG)", wiring{host: "h2", uplinks: []string{"vmnic1", "vmnic3"}}, "wl/untagged", nil},
		{"uplinks span underlays", wiring{host: "h1", uplinks: []string{"vmnic1", "vmnic2"}}, "", ErrUplinksSpanUnderlays},
		{"trunk not simulated", wiring{host: "h1", uplinks: []string{"vmnic1"}, trunk: true}, "", ErrVlanModeNotSimulated},
		{"pvlan not simulated", wiring{host: "h1", uplinks: []string{"vmnic1"}, pvlan: true}, "", ErrVlanModeNotSimulated},
		{"vlan out of range", wiring{host: "h1", uplinks: []string{"vmnic1"}, vlan: 5000}, "", errAny},
		{"nsx segment ignores vlan", wiring{host: "h1", uplinks: []string{"vmnic1"}, segment: "seg-9"}, "seg:seg-9", nil},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d, err := topo.resolve(tc.w)
			switch {
			case tc.err == errAny && err == nil:
				t.Fatal("want error")
			case tc.err == errAny:
				return
			case tc.err != nil:
				if !errors.Is(err, tc.err) {
					t.Fatalf("error %v, want %v", err, tc.err)
				}
				return
			case err != nil:
				t.Fatal(err)
			}
			if got := d.Key(); got != tc.key {
				t.Errorf("key %q, want %q", got, tc.key)
			}
		})
	}
}

func TestResolveVNICInventory(t *testing.T) {
	topo := resolveTopology(t)
	ctx := &Context{Map: NewRegistry()}

	dvsRef := types.ManagedObjectReference{Type: "VmwareDistributedVirtualSwitch", Value: "dvs-1"}
	dvs := &VmwareDistributedVirtualSwitch{VmwareDistributedVirtualSwitch: mo.VmwareDistributedVirtualSwitch{
		DistributedVirtualSwitch: mo.DistributedVirtualSwitch{
			ManagedEntity: mo.ManagedEntity{ExtensibleManagedObject: mo.ExtensibleManagedObject{Self: dvsRef}},
			Uuid:          "dvs-1",
		},
	}}
	ctx.Map.Put(dvs)

	pgRef := types.ManagedObjectReference{Type: "DistributedVirtualPortgroup", Value: "dvpg-10"}
	dvpg := &DistributedVirtualPortgroup{mo.DistributedVirtualPortgroup{
		Network: mo.Network{ManagedEntity: mo.ManagedEntity{ExtensibleManagedObject: mo.ExtensibleManagedObject{Self: pgRef}}},
		Config: types.DVPortgroupConfigInfo{
			DistributedVirtualSwitch: &dvsRef,
			DefaultPortConfig: &types.VMwareDVSPortSetting{
				Vlan: &types.VmwareDistributedVirtualSwitchVlanIdSpec{VlanId: 10},
			},
		},
	}}
	ctx.Map.Put(dvpg)

	nsxRef := types.ManagedObjectReference{Type: "DistributedVirtualPortgroup", Value: "dvpg-nsx"}
	nsx := &DistributedVirtualPortgroup{mo.DistributedVirtualPortgroup{
		Network: mo.Network{ManagedEntity: mo.ManagedEntity{ExtensibleManagedObject: mo.ExtensibleManagedObject{Self: nsxRef}}},
		Config: types.DVPortgroupConfigInfo{
			DistributedVirtualSwitch: &dvsRef,
			LogicalSwitchUuid:        "lsw-1",
		},
	}}
	ctx.Map.Put(nsx)

	stdRef := types.ManagedObjectReference{Type: "Network", Value: "VM Network"}
	std := &mo.Network{ManagedEntity: mo.ManagedEntity{ExtensibleManagedObject: mo.ExtensibleManagedObject{Self: stdRef}}, Name: "VM Network"}
	ctx.Map.Put(std)

	opRef := types.ManagedObjectReference{Type: "OpaqueNetwork", Value: "opaque-1"}
	op := &mo.OpaqueNetwork{Network: mo.Network{
		ManagedEntity: mo.ManagedEntity{ExtensibleManagedObject: mo.ExtensibleManagedObject{Self: opRef}},
		Summary:       &types.OpaqueNetworkSummary{OpaqueNetworkId: "seg-op"},
	}}
	ctx.Map.Put(op)

	host := &HostSystem{HostSystem: mo.HostSystem{
		ManagedEntity: mo.ManagedEntity{ExtensibleManagedObject: mo.ExtensibleManagedObject{Self: types.ManagedObjectReference{Type: "HostSystem", Value: "h1"}}, Name: "h1"},
		Config: &types.HostConfigInfo{Network: &types.HostNetworkInfo{
			ProxySwitch: []types.HostProxySwitch{{DvsUuid: "dvs-1", Pnic: []string{"key-vim.host.PhysicalNic-vmnic1"}}},
			Vswitch:     []types.HostVirtualSwitch{{Name: "vSwitch0", Pnic: []string{"key-vim.host.PhysicalNic-vmnic0"}}},
			Portgroup:   []types.HostPortGroup{{Spec: types.HostPortGroupSpec{Name: "VM Network", VswitchName: "vSwitch0", VlanId: 0}}},
		}},
	}}

	tests := []struct {
		name    string
		network mo.Reference
		key     string
		err     error
	}{
		{"dvpg vlan 10 on vmnic1", dvpg, "wl/v10", nil},
		{"nsx dvpg", nsx, "seg:lsw-1", nil},
		{"standard portgroup on vSwitch0 uses vmnic0 default", std, "mgmt/untagged", nil},
		{"opaque network on vmnic1", op, "seg:seg-op", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d, err := topo.ResolveVNIC(ctx, host, tc.network)
			if err != nil {
				t.Fatal(err)
			}
			if got := d.Key(); got != tc.key {
				t.Errorf("key %q, want %q", got, tc.key)
			}
		})
	}
}

func TestModelRouterSeamsNeedTopology(t *testing.T) {
	m := VPX()
	if err := m.Create(); err != nil {
		t.Fatal(err)
	}
	defer m.Remove()

	if _, err := m.ListenRouter("gw", "127.0.0.1:0"); !errors.Is(err, ErrNoTopology) {
		t.Errorf("ListenRouter without a topology: %v, want ErrNoTopology", err)
	}
	if _, err := m.DialRouter("gw", "127.0.0.1:0"); !errors.Is(err, ErrNoTopology) {
		t.Errorf("DialRouter without a topology: %v, want ErrNoTopology", err)
	}
}
