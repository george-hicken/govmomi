// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package simulator

import (
	"strconv"
	"strings"
	"testing"

	"github.com/vmware/govmomi/vim25/types"
)

// moIDSuffixes maps every numeric-suffixed MoID in a freshly created VPX model to its suffix.
func moIDSuffixes(t *testing.T, base int64) map[types.ManagedObjectReference]int64 {
	t.Helper()
	m := VPX()
	m.MoIDBase = base
	if err := m.Create(); err != nil {
		t.Fatal(err)
	}
	defer m.Remove()
	r := m.Map()
	r.m.Lock()
	defer r.m.Unlock()
	ns := map[types.ManagedObjectReference]int64{}
	for ref := range r.objects {
		n, err := strconv.ParseInt(strings.TrimPrefix(ref.Value, valuePrefix(ref.Type)), 10, 64)
		if err == nil {
			ns[ref] = n
		}
	}
	return ns
}

// TestModelMoIDBase: every generated MoID lies above MoIDBase, starting at MoIDBase+1. MoIDs fixed by
// the model's templates (the vpx alarm definitions) are the same in every model and are not generated.
func TestModelMoIDBase(t *testing.T) {
	const base = 1_000_000
	fixed := moIDSuffixes(t, 0)
	got := moIDSuffixes(t, base)

	first := false
	for ref, n := range got {
		if _, ok := fixed[ref]; ok {
			continue
		}
		if n <= base || n > 2*base {
			t.Errorf("%s outside (%d,%d]", ref, base, 2*base)
		}
		first = first || n == base+1
	}
	if !first {
		t.Errorf("no MoID with suffix %d", base+1)
	}
}
