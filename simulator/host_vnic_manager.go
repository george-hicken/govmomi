// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package simulator

import (
	"errors"
	"fmt"
	"slices"

	"github.com/vmware/govmomi/simulator/esx"
	"github.com/vmware/govmomi/vim25/methods"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/soap"
	"github.com/vmware/govmomi/vim25/types"
)

type HostVirtualNicManager struct {
	mo.HostVirtualNicManager

	Host *mo.HostSystem
}

// NewHostVirtualNicManager returns a manager whose selections are its own. The default NetConfig, with vmk0
// selected for "management", is copied so that selecting or deselecting a vNIC cannot change another host's.
func NewHostVirtualNicManager(host *mo.HostSystem) *HostVirtualNicManager {
	netConfig := make([]types.VirtualNicManagerNetConfig, len(esx.VirtualNicManagerNetConfig))
	for i, c := range esx.VirtualNicManagerNetConfig {
		c.CandidateVnic = slices.Clone(c.CandidateVnic)
		c.SelectedVnic = slices.Clone(c.SelectedVnic)
		netConfig[i] = c
	}

	return &HostVirtualNicManager{
		Host: host,
		HostVirtualNicManager: mo.HostVirtualNicManager{
			Info: types.HostVirtualNicManagerInfo{
				NetConfig: netConfig,
			},
		},
	}
}

func (m *HostVirtualNicManager) QueryNetConfig(req *types.QueryNetConfig) soap.HasFault {
	body := new(methods.QueryNetConfigBody)

	for _, c := range m.Info.NetConfig {
		if c.NicType == req.NicType {
			body.Res = &types.QueryNetConfigResponse{
				Returnval: &c,
			}
			return body
		}
	}

	body.Fault_ = Fault("", &types.InvalidArgument{InvalidProperty: req.NicType})

	return body
}

// SelectVnicForNicType selects the candidate vNIC device for nicType. A nicType that does not allow multiple
// selections replaces its selection, as ESX does.
func (m *HostVirtualNicManager) SelectVnicForNicType(req *types.SelectVnicForNicType) soap.HasFault {
	body := new(methods.SelectVnicForNicTypeBody)

	cfg, key, err := m.candidate(req.NicType, req.Device)
	if err != nil {
		body.Fault_ = Fault(err.Error(), &types.InvalidArgument{InvalidProperty: req.Device})
		return body
	}

	if !cfg.MultiSelectAllowed {
		cfg.SelectedVnic = nil
	}
	if !slices.Contains(cfg.SelectedVnic, key) {
		cfg.SelectedVnic = append(cfg.SelectedVnic, key)
	}
	m.syncHost(req.NicType)

	body.Res = &types.SelectVnicForNicTypeResponse{}
	return body
}

// DeselectVnicForNicType removes the candidate vNIC device from the selections of nicType.
func (m *HostVirtualNicManager) DeselectVnicForNicType(req *types.DeselectVnicForNicType) soap.HasFault {
	body := new(methods.DeselectVnicForNicTypeBody)

	cfg, key, err := m.candidate(req.NicType, req.Device)
	if err != nil {
		body.Fault_ = Fault(err.Error(), &types.InvalidArgument{InvalidProperty: req.Device})
		return body
	}

	cfg.SelectedVnic = slices.DeleteFunc(slices.Clone(cfg.SelectedVnic), func(k string) bool { return k == key })
	m.syncHost(req.NicType)

	body.Res = &types.DeselectVnicForNicTypeResponse{}
	return body
}

// candidate returns the NetConfig entry for nicType and the key of its candidate vNIC device.
func (m *HostVirtualNicManager) candidate(nicType, device string) (*types.VirtualNicManagerNetConfig, string, error) {
	for i := range m.Info.NetConfig {
		cfg := &m.Info.NetConfig[i]
		if cfg.NicType != nicType {
			continue
		}
		for _, c := range cfg.CandidateVnic {
			if c.Device == device {
				return cfg, c.Key, nil
			}
		}
		return nil, "", fmt.Errorf("device %s is not a candidate for nicType %s", device, nicType)
	}

	return nil, "", errors.New("no NetConfig for nicType " + nicType)
}

// syncHost copies nicType's selection into the host's own NetConfig, which the rest of the simulator reads, so
// that the host and its vNIC manager agree.
func (m *HostVirtualNicManager) syncHost(nicType string) {
	if m.Host == nil || m.Host.Config == nil {
		return
	}
	var selected []string
	for _, c := range m.Info.NetConfig {
		if c.NicType == nicType {
			selected = c.SelectedVnic
		}
	}
	for i := range m.Host.Config.VirtualNicManagerInfo.NetConfig {
		if c := &m.Host.Config.VirtualNicManagerInfo.NetConfig[i]; c.NicType == nicType {
			c.SelectedVnic = slices.Clone(selected)
		}
	}
}
