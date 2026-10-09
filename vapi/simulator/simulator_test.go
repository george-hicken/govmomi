// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package simulator_test

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/vmware/govmomi/find"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/property"
	"github.com/vmware/govmomi/session"
	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/vapi/library"
	"github.com/vmware/govmomi/vapi/rest"
	"github.com/vmware/govmomi/vapi/tags"
	"github.com/vmware/govmomi/vapi/vcenter"
	"github.com/vmware/govmomi/vim25"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/soap"
	"github.com/vmware/govmomi/vim25/types"

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

func TestLibraryItemSecurityCompliance(t *testing.T) {
	simulator.Test(func(ctx context.Context, vc *vim25.Client) {
		m, id := newLibrary(ctx, t, vc)

		policy, err := m.DefaultOvfSecurityPolicy(ctx)
		if err != nil {
			t.Fatal(err)
		}
		l, err := m.GetLibraryByID(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		secure, err := m.CreateLibrary(ctx, library.Library{
			Name:             "secure",
			Type:             "LOCAL",
			Storage:          l.Storage,
			SecurityPolicyID: policy,
		})
		if err != nil {
			t.Fatal(err)
		}

		tests := []struct {
			name      string
			libraryID string
			compliant bool
			cert      bool
		}{
			{"no policy", id, true, false},
			{"policy", secure, false, true},
		}
		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				itemID, err := m.CreateLibraryItem(ctx, library.Item{Name: "item", Type: library.ItemTypeOVF, LibraryID: test.libraryID})
				if err != nil {
					t.Fatal(err)
				}
				item, err := m.GetLibraryItem(ctx, itemID)
				if err != nil {
					t.Fatal(err)
				}
				if item.SecurityCompliance == nil || *item.SecurityCompliance != test.compliant {
					t.Errorf("SecurityCompliance=%v, expected %t", item.SecurityCompliance, test.compliant)
				}
				if (item.CertificateVerification != nil) != test.cert {
					t.Errorf("CertificateVerification=%v, expected present=%t", item.CertificateVerification, test.cert)
				}
			})
		}
	})
}

// TestLibraryItemOVFFilterAndDeployNetwork checks that the ovf_summary of an OVF item
// lists the descriptor's networks and annotation, and that a deploy mapping that
// network to a distributed portgroup backs the VM's NIC with that portgroup.
func TestLibraryItemOVFFilterAndDeployNetwork(t *testing.T) {
	simulator.Test(func(ctx context.Context, vc *vim25.Client) {
		m, libID := newLibrary(ctx, t, vc)

		b, err := os.ReadFile("../../ovf/fixtures/ttylinux.ovf")
		if err != nil {
			t.Fatal(err)
		}
		desc := strings.ReplaceAll(string(b), `ovf:name="nat"`, `ovf:name="Management Network"`)
		desc = strings.ReplaceAll(desc, "<rasd:Connection>nat</rasd:Connection>", "<rasd:Connection>Management Network</rasd:Connection>")
		desc = strings.Replace(desc, "<Name>ttylinux-pc_i486-16.1</Name>",
			"<Name>ttylinux-pc_i486-16.1</Name>\n<AnnotationSection><Info>A human-readable annotation</Info><Annotation>tiny linux</Annotation></AnnotationSection>", 1)

		itemID, err := m.CreateLibraryItem(ctx, library.Item{Name: "ttylinux", Type: library.ItemTypeOVF, LibraryID: libID})
		if err != nil {
			t.Fatal(err)
		}
		session, err := m.CreateLibraryItemUpdateSession(ctx, library.Session{LibraryItemID: itemID})
		if err != nil {
			t.Fatal(err)
		}
		update, err := m.AddLibraryItemFile(ctx, session, library.UpdateFile{
			Name:       "ttylinux.ovf",
			SourceType: "PUSH",
			Size:       int64(len(desc)),
		})
		if err != nil {
			t.Fatal(err)
		}
		u, err := url.Parse(update.UploadEndpoint.URI)
		if err != nil {
			t.Fatal(err)
		}
		p := soap.DefaultUpload
		p.ContentLength = int64(len(desc))
		if err = m.Client.Upload(ctx, strings.NewReader(desc), u, &p); err != nil {
			t.Fatal(err)
		}
		if err = m.CompleteLibraryItemUpdateSession(ctx, session); err != nil {
			t.Fatal(err)
		}

		vm := vcenter.NewManager(m.Client)
		filter, err := vm.FilterLibraryItem(ctx, itemID, vcenter.FilterRequest{})
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(filter.Networks, []string{"Management Network"}) {
			t.Errorf("Networks = %q, want [Management Network]", filter.Networks)
		}
		if filter.Annotation != "tiny linux" {
			t.Errorf("Annotation = %q, want %q", filter.Annotation, "tiny linux")
		}
		if filter.Name != "ttylinux" {
			t.Errorf("Name = %q, want ttylinux", filter.Name)
		}

		finder := find.NewFinder(vc)
		dc, err := finder.DefaultDatacenter(ctx)
		if err != nil {
			t.Fatal(err)
		}
		finder.SetDatacenter(dc)
		pool, err := finder.ResourcePool(ctx, "DC0_C0/Resources")
		if err != nil {
			t.Fatal(err)
		}
		dvpg, err := finder.Network(ctx, "DC0_DVPG0")
		if err != nil {
			t.Fatal(err)
		}

		ref, err := vm.DeployLibraryItem(ctx, itemID, vcenter.Deploy{
			DeploymentSpec: vcenter.DeploymentSpec{
				Name: "ttylinux-dvpg",
				NetworkMappings: []vcenter.NetworkMapping{{
					Key:   filter.Networks[0],
					Value: dvpg.Reference().Value,
				}},
			},
			Target: vcenter.Target{ResourcePoolID: pool.Reference().Value},
		})
		if err != nil {
			t.Fatal(err)
		}

		var obj mo.VirtualMachine
		if err = property.DefaultCollector(vc).RetrieveOne(ctx, *ref, []string{"config.hardware.device", "network"}, &obj); err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(obj.Network, dvpg.Reference()) {
			t.Errorf("VM network = %v, want it to include %s", obj.Network, dvpg.Reference())
		}
		nics := object.VirtualDeviceList(obj.Config.Hardware.Device).SelectByType((*types.VirtualEthernetCard)(nil))
		if len(nics) != 1 {
			t.Fatalf("got %d NICs, want 1", len(nics))
		}
		backing, ok := nics[0].GetVirtualDevice().Backing.(*types.VirtualEthernetCardDistributedVirtualPortBackingInfo)
		if !ok {
			t.Fatalf("NIC backing = %T, want a distributed port backing", nics[0].GetVirtualDevice().Backing)
		}
		if backing.Port.PortgroupKey != dvpg.Reference().Value {
			t.Errorf("NIC portgroup key = %q, want %q", backing.Port.PortgroupKey, dvpg.Reference().Value)
		}
	})
}

// TestSOAPSessionSharedWithREST checks that a SOAP session ID presented as
// vmware-api-session-id authorizes vAPI calls without a vAPI login, as vCenter
// 8.0U3 and later allow; that an unknown ID is rejected; and that a SOAP logout
// ends the shared session.
func TestSOAPSessionSharedWithREST(t *testing.T) {
	simulator.Test(func(ctx context.Context, vc *vim25.Client) {
		cookie := vc.SessionCookie()
		if cookie == nil {
			t.Fatal("no SOAP session cookie")
		}

		c := rest.NewClient(vc)
		c.SessionID(cookie.Value)

		vm := simulator.Map(ctx).Any("VirtualMachine").Reference()
		m := tags.NewManager(c)
		if _, err := m.ListAttachedTags(ctx, vm); err != nil {
			t.Fatalf("ListAttachedTags with the SOAP session ID: %s", err)
		}
		s, err := c.Session(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if s == nil || s.User != simulator.DefaultLogin.Username() {
			t.Errorf("session = %+v, want user %q", s, simulator.DefaultLogin.Username())
		}

		unknown := rest.NewClient(vc)
		unknown.SessionID("not-a-session")
		if _, err := tags.NewManager(unknown).ListAttachedTags(ctx, vm); !rest.IsStatusError(err, http.StatusUnauthorized) {
			t.Errorf("ListAttachedTags with an unknown session ID: err = %v, want 401", err)
		}

		if err := session.NewManager(vc).Logout(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := m.ListAttachedTags(ctx, vm); !rest.IsStatusError(err, http.StatusUnauthorized) {
			t.Errorf("ListAttachedTags after SOAP logout: err = %v, want 401", err)
		}
	})
}
