// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package simulator

import (
	"context"
	"testing"

	"github.com/vmware/govmomi/vim25"
	"github.com/vmware/govmomi/vim25/methods"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/soap"
	"github.com/vmware/govmomi/vim25/types"
)

// ticketVM adds a method to one VM per call, as an extension does through Registry.Handler.
type ticketVM struct {
	*VirtualMachine
}

func (vm *ticketVM) AcquireTicket(req *types.AcquireTicket) soap.HasFault {
	return &methods.AcquireTicketBody{Res: &types.AcquireTicketResponse{Returnval: types.VirtualMachineTicket{Ticket: req.TicketType}}}
}

func TestRegistryHandlerWinsOverSession(t *testing.T) {
	model := VPX()
	err := model.Run(func(ctx context.Context, c *vim25.Client) error {
		m := model.Map()
		m.Handler = func(ctx *Context, method *Method) (mo.Reference, types.BaseMethodFault) {
			if vm, ok := ctx.Map.Get(method.This).(*VirtualMachine); ok && method.Name == "AcquireTicket" {
				return &ticketVM{vm}, nil
			}
			return nil, nil
		}
		vm := m.Any("VirtualMachine")
		res, err := methods.AcquireTicket(ctx, c, &types.AcquireTicket{This: vm.Reference(), TicketType: "mks"})
		if err != nil {
			return err // the session's *VirtualMachine, which lacks AcquireTicket, was dispatched
		}
		if res.Returnval.Ticket != "mks" {
			t.Errorf("ticket %q", res.Returnval.Ticket)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
