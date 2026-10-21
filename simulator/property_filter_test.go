// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package simulator

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/vmware/govmomi/property"
	"github.com/vmware/govmomi/vim25/types"
)

// TestPropertyFilter_NestedPathParentChange checks that a filter on a nested
// property reports a change that PropertyDiff, and so ctx.AutoUpdate, reports
// at its top-level parent.
func TestPropertyFilter_NestedPathParentChange(t *testing.T) {
	env := newVCSIMEnv(t)
	vm := allVMs(t, env)[0]

	ctx, cancel := context.WithTimeout(env.goCtx, 10*time.Second)
	defer cancel()

	go func() {
		time.Sleep(200 * time.Millisecond)
		env.simCtx.AutoUpdate(vm, func() {
			vm.Guest.HostName = "nested-change"
		})
	}()

	var got any
	err := property.Wait(ctx, env.pc, vm.Self, []string{"guest.hostName"}, func(changes []types.PropertyChange) bool {
		for _, c := range changes {
			require.Equal(t, "guest.hostName", c.Name)
			got = c.Val
		}
		return got == "nested-change"
	})
	require.NoError(t, err, "got %v", got)
}

// TestPropertyFilter_MultiPathParentChange checks that a change to a parent
// property, as PropertyDiff reports it, is reported for every requested path
// under that parent, not just the first one listed.
func TestPropertyFilter_MultiPathParentChange(t *testing.T) {
	env := newVCSIMEnv(t)
	vm := allVMs(t, env)[0]

	ctx, cancel := context.WithTimeout(env.goCtx, 10*time.Second)
	defer cancel()

	go func() {
		time.Sleep(200 * time.Millisecond)
		env.simCtx.AutoUpdate(vm, func() {
			vm.Guest.HostName = "multi-change"
			vm.Guest.IpAddress = "10.0.2.102"
		})
	}()

	got := make(map[string]any)
	err := property.Wait(ctx, env.pc, vm.Self, []string{"guest.hostName", "guest.ipAddress"}, func(changes []types.PropertyChange) bool {
		for _, c := range changes {
			got[c.Name] = c.Val
		}
		return got["guest.hostName"] == "multi-change" && got["guest.ipAddress"] == "10.0.2.102"
	})
	require.NoError(t, err, "got %v", got)
}
