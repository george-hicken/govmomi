// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package simulator_test

import (
	"context"
	"testing"

	"github.com/vmware/govmomi/find"
	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/vapi/library"
	"github.com/vmware/govmomi/vapi/rest"
	"github.com/vmware/govmomi/vim25"

	_ "github.com/vmware/govmomi/vapi/simulator"
)

// newLibrary logs in to the vAPI endpoint and creates a local library on the default datastore.
func newLibrary(ctx context.Context, t *testing.T, vc *vim25.Client) (*library.Manager, string) {
	t.Helper()

	c := rest.NewClient(vc)
	if err := c.Login(ctx, simulator.DefaultLogin); err != nil {
		t.Fatal(err)
	}

	ds, err := find.NewFinder(vc).DefaultDatastore(ctx)
	if err != nil {
		t.Fatal(err)
	}

	m := library.NewManager(c)
	id, err := m.CreateLibrary(ctx, library.Library{
		Name: "example",
		Type: "LOCAL",
		Storage: []library.StorageBacking{{
			DatastoreID: ds.Reference().Value,
			Type:        "DATASTORE",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	return m, id
}

func TestLibraryVersion(t *testing.T) {
	simulator.Test(func(ctx context.Context, vc *vim25.Client) {
		m, id := newLibrary(ctx, t, vc)

		version := func(expect string) *library.Library {
			t.Helper()
			l, err := m.GetLibraryByID(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if l.Version != expect {
				t.Fatalf("library version=%q, expected %q", l.Version, expect)
			}
			return l
		}

		l := version("1")

		// Item changes leave the library version unchanged.
		itemID, err := m.CreateLibraryItem(ctx, library.Item{Name: "item", Type: library.ItemTypeISO, LibraryID: id})
		if err != nil {
			t.Fatal(err)
		}
		item, err := m.GetLibraryItem(ctx, itemID)
		if err != nil {
			t.Fatal(err)
		}
		item.Name = "renamed"
		if err = m.UpdateLibraryItem(ctx, item); err != nil {
			t.Fatal(err)
		}
		if err = m.DeleteLibraryItem(ctx, item); err != nil {
			t.Fatal(err)
		}
		version("1")

		// An update of the library's own properties increments it.
		l.Name = "renamed"
		if err = m.UpdateLibrary(ctx, l); err != nil {
			t.Fatal(err)
		}
		version("2")
	})
}
