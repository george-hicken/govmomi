// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package simulator_test

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vmware/govmomi"
	"github.com/vmware/govmomi/lookup"
	lookuptypes "github.com/vmware/govmomi/lookup/types"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/property"
	"github.com/vmware/govmomi/session"
	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/vapi/rest"
	"github.com/vmware/govmomi/vapi/tags"
	"github.com/vmware/govmomi/vapi/vcenter/consumptiondomains/zones"
	"github.com/vmware/govmomi/view"
	"github.com/vmware/govmomi/vim25"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/soap"
	"github.com/vmware/govmomi/vim25/types"

	_ "github.com/vmware/govmomi/lookup/simulator"
	_ "github.com/vmware/govmomi/vapi/simulator"
	_ "github.com/vmware/govmomi/vapi/vcenter/consumptiondomains/simulator"
)

// isoModel is a VPX (or ESX) Model served over HTTP with a logged-in govmomi client.
//
// Test isolation is judged by InstanceUuid and by content, not by MoID. Two Models have the same
// MoID set (for example group-d1 and vm-N) by construction, so MoIDs cannot tell them apart.
type isoModel struct {
	t   *testing.T
	ctx context.Context
	m   *simulator.Model
	s   *simulator.Server
	c   *govmomi.Client
}

func newIsoModel(t *testing.T, m *simulator.Model) *isoModel {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, m.Create())
	t.Cleanup(m.Remove)

	// The vapi, lookup and consumption-domains handlers are registered only when RegisterEndpoints is set,
	// as Model.Run does.
	m.Service.RegisterEndpoints = true
	s := m.Service.NewServer()
	t.Cleanup(s.Close)

	c, err := govmomi.NewClient(ctx, s.URL, true)
	require.NoError(t, err)

	return &isoModel{t: t, ctx: ctx, m: m, s: s, c: c}
}

// newIsoVPX returns a VPX Model, created and served, with a logged-in client.
func newIsoVPX(t *testing.T) *isoModel {
	t.Helper()
	return newIsoModel(t, simulator.VPX())
}

// at returns a copy of the model that reports failures to t, for use inside a subtest.
func (x *isoModel) at(t *testing.T) *isoModel {
	y := *x
	y.t = t
	return &y
}

// login returns a new govmomi client to the same server, logged in with the default credentials.
func (x *isoModel) login() *govmomi.Client {
	x.t.Helper()
	c, err := govmomi.NewClient(x.ctx, x.s.URL, true)
	require.NoError(x.t, err)
	return c
}

// namesOf returns the sorted names of objects of kind, from an arbitrary vim25 client (for example the
// in-memory client).
func (x *isoModel) namesOf(c *vim25.Client, kind string) []string {
	x.t.Helper()
	var out []string
	for _, e := range entities(x.t, x.ctx, c, kind) {
		out = append(out, e.Name)
	}
	slices.Sort(out)
	return out
}

// entities returns the objects of the given kind reachable from the root folder of client c.
func entities(t *testing.T, ctx context.Context, c *vim25.Client, kind string) []mo.ManagedEntity {
	t.Helper()
	v, err := view.NewManager(c).CreateContainerView(ctx, c.ServiceContent.RootFolder, []string{kind}, true)
	require.NoError(t, err)
	defer func() { _ = v.Destroy(ctx) }()

	var dst []mo.ManagedEntity
	require.NoError(t, v.Retrieve(ctx, []string{kind}, []string{"name"}, &dst))
	return dst
}

// names returns the names of all objects of the given kind visible to the model's client.
func (x *isoModel) names(kind string) []string {
	x.t.Helper()
	var out []string
	for _, e := range entities(x.t, x.ctx, x.c.Client, kind) {
		out = append(out, e.Name)
	}
	slices.Sort(out)
	return out
}

// refs returns the sorted MoIDs of every ManagedEntity in the model.
func (x *isoModel) refs() []string {
	x.t.Helper()
	var out []string
	for _, e := range entities(x.t, x.ctx, x.c.Client, "ManagedEntity") {
		out = append(out, e.Self.String())
	}
	slices.Sort(out)
	return out
}

// refOf returns the reference of the first object of kind named name.
func (x *isoModel) refOf(kind, name string) types.ManagedObjectReference {
	x.t.Helper()
	for _, e := range entities(x.t, x.ctx, x.c.Client, kind) {
		if e.Name == name {
			return e.Self
		}
	}
	require.Failf(x.t, "object not found", "%s %q", kind, name)
	return types.ManagedObjectReference{}
}

// datacenterFolders returns the folders of the first Datacenter in the model.
func (x *isoModel) datacenterFolders() *object.DatacenterFolders {
	x.t.Helper()
	dc := object.NewDatacenter(x.c.Client, x.refOf("Datacenter", "DC0"))
	folders, err := dc.Folders(x.ctx)
	require.NoError(x.t, err)
	return folders
}

// TestTwoVPXModelsIsolation runs two VPX Models in one process and checks that inventory, sessions,
// property collection, vapi state, zones, lookup registrations and template-derived objects do not
// leak between them. It sets no process-wide knob (Trace, TraceFile, DefaultLogin).
//
// The subtests share the two Models and run in order: identity first, then mutations.
func TestTwoVPXModelsIsolation(t *testing.T) {
	a0 := newIsoVPX(t)
	b0 := newIsoVPX(t)

	// Names and refs are captured before any subtest renames an object.
	vmNames := a0.names("VirtualMachine")
	require.GreaterOrEqual(t, len(vmNames), 2)
	vm0, vm1 := vmNames[0], vmNames[1]
	vm0Ref := a0.refOf("VirtualMachine", vm0)
	require.Equal(t, vm0Ref, b0.refOf("VirtualMachine", vm0), "same MoID in both Models")
	hostNames := a0.names("HostSystem")
	require.NotEmpty(t, hostNames)
	hostRefA := a0.refOf("HostSystem", hostNames[0])

	t.Run("instance uuid differs", func(t *testing.T) {
		a, b := a0.at(t), b0.at(t)
		require.NotEmpty(t, a.c.ServiceContent.About.InstanceUuid)
		require.NotEqual(t, a.c.ServiceContent.About.InstanceUuid, b.c.ServiceContent.About.InstanceUuid)
	})

	t.Run("moid sets are identical, moid is not identity", func(t *testing.T) {
		a, b := a0.at(t), b0.at(t)
		// Same topology, same MoIDs. Anything keyed on a MoID alone collides between the two.
		require.Equal(t, a.refs(), b.refs())
		require.NotEmpty(t, a.refs())
	})

	t.Run("inventory mutation in A is invisible in B", func(t *testing.T) {
		a, b := a0.at(t), b0.at(t)
		vmBefore := b.names("VirtualMachine")
		folderBefore := b.names("Folder")

		folder, err := a.datacenterFolders().VmFolder.CreateFolder(a.ctx, "a-only-folder")
		require.NoError(t, err)
		require.NotNil(t, folder)

		task, err := object.NewCommon(a.c.Client, vm0Ref).Rename(a.ctx, "renamed-a")
		require.NoError(t, err)
		require.NoError(t, task.Wait(a.ctx))

		require.Contains(t, a.names("Folder"), "a-only-folder")
		require.Contains(t, a.names("VirtualMachine"), "renamed-a")

		require.Equal(t, folderBefore, b.names("Folder"))
		require.Equal(t, vmBefore, b.names("VirtualMachine"))
		require.NotContains(t, b.names("Folder"), "a-only-folder")
		require.NotContains(t, b.names("VirtualMachine"), "renamed-a")
	})

	t.Run("logout in A leaves B working and A cookie rejected by B", func(t *testing.T) {
		a, b := a0.at(t), b0.at(t)
		ctx := a.ctx
		keyA, err := a.c.SessionManager.UserSession(ctx)
		require.NoError(t, err)
		require.NotEmpty(t, keyA.Key)

		keyB, err := b.c.SessionManager.UserSession(ctx)
		require.NoError(t, err)
		require.NotEqual(t, keyA.Key, keyB.Key)

		require.NoError(t, a.c.Logout(ctx))
		sessA, err := a.c.SessionManager.UserSession(ctx)
		require.NoError(t, err)
		require.Nil(t, sessA, "A session must be gone after logout")

		sessB, err := b.c.SessionManager.UserSession(ctx)
		require.NoError(t, err)
		require.NotNil(t, sessB, "B session must survive A logout")
		require.NotEmpty(t, b.names("VirtualMachine"))

		// Present A's (now dead) key to B's server. B has no record of it.
		sc := soap.NewClient(b.s.URL, true)
		sc.Jar.SetCookies(b.s.URL, []*http.Cookie{{Name: soap.SessionCookieName, Value: keyA.Key}})
		vc, err := vim25.NewClient(ctx, sc)
		require.NoError(t, err)
		// UserSession reports an unauthenticated client as (nil, nil), so also require an authenticated call to fail.
		sessRejected, err := session.NewManager(vc).UserSession(ctx)
		require.NoError(t, err)
		require.Nil(t, sessRejected, "A cookie must not authenticate on B")
		_, err = view.NewManager(vc).CreateContainerView(ctx, vc.ServiceContent.RootFolder, []string{"Folder"}, true)
		require.Error(t, err, "A cookie must be rejected by B")
	})

	// A is logged out; later subtests need a fresh client for it.
	a0.c = a0.at(t).login()

	t.Run("property collector wait on A sees only A changes", func(t *testing.T) {
		a, b := a0.at(t), b0.at(t)
		ctx, cancel := context.WithTimeout(a.ctx, 30*time.Second)
		defer cancel()

		var mu sync.Mutex
		var seen []string
		done := make(chan error, 1)

		filter := new(property.WaitFilter)
		filter.Add(vm0Ref, "VirtualMachine", []string{"name"})
		go func() {
			done <- property.WaitForUpdatesEx(ctx, property.DefaultCollector(a.c.Client), filter, func(updates []types.ObjectUpdate) bool {
				mu.Lock()
				defer mu.Unlock()
				for _, u := range updates {
					for _, c := range u.ChangeSet {
						if name, ok := c.Val.(string); ok && c.Name == "name" {
							seen = append(seen, name)
						}
					}
				}
				return slices.Contains(seen, "renamed-wait-a")
			})
		}()

		// B renames the object with the same MoID first. A must not see it.
		taskB, err := object.NewCommon(b.c.Client, vm0Ref).Rename(b.ctx, "renamed-wait-b")
		require.NoError(t, err)
		require.NoError(t, taskB.Wait(b.ctx))

		taskA, err := object.NewCommon(a.c.Client, vm0Ref).Rename(a.ctx, "renamed-wait-a")
		require.NoError(t, err)
		require.NoError(t, taskA.Wait(a.ctx))

		require.NoError(t, <-done)
		mu.Lock()
		defer mu.Unlock()
		require.Contains(t, seen, "renamed-wait-a")
		require.NotContains(t, seen, "renamed-wait-b", "B's change leaked into A's property collector")
	})

	t.Run("vapi session, tag and zone in A are invisible in B", func(t *testing.T) {
		a, b := a0.at(t), b0.at(t)
		rcA := rest.NewClient(a.c.Client)
		require.NoError(t, rcA.Login(a.ctx, a.s.URL.User))
		tmA := tags.NewManager(rcA)

		catID, err := tmA.CreateCategory(a.ctx, &tags.Category{
			Name:            "iso-cat",
			Description:     "A only",
			Cardinality:     "MULTIPLE",
			AssociableTypes: []string{"VirtualMachine"},
		})
		require.NoError(t, err)
		tagID, err := tmA.CreateTag(a.ctx, &tags.Tag{Name: "iso-tag", CategoryID: catID})
		require.NoError(t, err)

		listA, err := tmA.ListTags(a.ctx)
		require.NoError(t, err)
		require.Contains(t, listA, tagID)

		rcB := rest.NewClient(b.c.Client)
		require.NoError(t, rcB.Login(b.ctx, b.s.URL.User))
		tmB := tags.NewManager(rcB)

		listB, err := tmB.ListTags(b.ctx)
		require.NoError(t, err)
		require.NotContains(t, listB, tagID)
		_, err = tmB.GetCategory(b.ctx, catID)
		require.Error(t, err, "A category must not resolve on B")

		// A's REST session id must not authenticate against B.
		rcBAsA := rest.NewClient(b.c.Client)
		rcBAsA.SessionID(rcA.SessionID())
		_, err = tags.NewManager(rcBAsA).ListTags(b.ctx)
		require.Error(t, err, "A REST session must be rejected by B")

		zA := zones.NewManager(rcA)
		_, err = zA.CreateZone(zones.CreateSpec{Zone: "zone-a", Description: "A only"})
		require.NoError(t, err)
		zonesA, err := zA.ListZones()
		require.NoError(t, err)
		require.Len(t, zonesA, 1)

		zonesB, err := zones.NewManager(rcB).ListZones()
		require.NoError(t, err)
		require.Empty(t, zonesB, "A zone must not appear in B")
	})

	t.Run("lookup registrations are per Model", func(t *testing.T) {
		a, b := a0.at(t), b0.at(t)
		filter := &lookuptypes.LookupServiceRegistrationFilter{
			ServiceType: &lookuptypes.LookupServiceRegistrationServiceType{Product: "com.vmware.cis", Type: "vcenterserver"},
		}
		list := func(x *isoModel) []lookuptypes.LookupServiceRegistrationInfo {
			lc, err := lookup.NewClient(x.ctx, x.c.Client)
			require.NoError(t, err)
			infos, err := lc.List(x.ctx, filter)
			require.NoError(t, err)
			return infos
		}

		infoA := list(a)
		infoB := list(b)
		require.Len(t, infoA, 1)
		require.Len(t, infoB, 1)

		require.Equal(t, a.c.ServiceContent.About.InstanceUuid, infoA[0].ServiceId)
		require.Equal(t, b.c.ServiceContent.About.InstanceUuid, infoB[0].ServiceId)
		require.NotEqual(t, infoA[0].NodeId, infoB[0].NodeId, "node ids must differ")
		require.NotEmpty(t, infoA[0].ServiceEndpoints)
		require.NotEmpty(t, infoB[0].ServiceEndpoints)
		require.Contains(t, infoA[0].ServiceEndpoints[0].Url, a.s.URL.Host, "A registration must point at A's server")
		require.Contains(t, infoB[0].ServiceEndpoints[0].Url, b.s.URL.Host, "B registration must point at B's server")
		require.NotEqual(t, infoA[0].ServiceEndpoints[0].Url, infoB[0].ServiceEndpoints[0].Url)
	})

	t.Run("in-memory client resolves its own Model", func(t *testing.T) {
		a, b := a0.at(t), b0.at(t)
		// Service.RoundTrip (no HTTP) runs against the Model's own Map. internalSession.Registry is process-wide.
		inA, err := vim25.NewClient(a.ctx, a.m.Service)
		require.NoError(t, err)
		inA.ServiceContent = a.c.ServiceContent

		inB, err := vim25.NewClient(b.ctx, b.m.Service)
		require.NoError(t, err)
		inB.ServiceContent = b.c.ServiceContent

		require.Contains(t, a.namesOf(inA, "Folder"), "a-only-folder")
		require.NotContains(t, b.namesOf(inB, "Folder"), "a-only-folder")
	})

	t.Run("template-derived objects are not shared", func(t *testing.T) {
		a, b := a0.at(t), b0.at(t)
		hsA := a.m.Map().Get(hostRefA).(*simulator.HostSystem)
		require.NotEmpty(t, hsA.Hardware.CpuPkg)

		hostB := b.m.Map().Get(hostRefA).(*simulator.HostSystem)
		before := hostB.Hardware.CpuPkg[0].Hz

		// NewHostSystem copies Hardware from the esx template. A mutation in A must not reach B or a fresh Model.
		hsA.Hardware.CpuPkg[0].Hz = before + 1
		defer func() { hsA.Hardware.CpuPkg[0].Hz = before }()

		require.Equal(t, before, hostB.Hardware.CpuPkg[0].Hz, "B host hardware shares A's mutation")

		c := newIsoVPX(t)
		hostC := c.m.Map().Get(hostRefA).(*simulator.HostSystem)
		require.Equal(t, before, hostC.Hardware.CpuPkg[0].Hz, "fresh Model shares A's mutation via the template")
	})

	t.Run("concurrent use under -race", func(t *testing.T) {
		a, b := a0.at(t), b0.at(t)
		folderA := a.datacenterFolders().VmFolder
		folderB := b.datacenterFolders().VmFolder
		vmRefA := a.refOf("VirtualMachine", vm1)
		vmRefB := b.refOf("VirtualMachine", vm1)

		const n = 5
		var wg sync.WaitGroup
		run := func(prefix string, folder *object.Folder, ref types.ManagedObjectReference, c *govmomi.Client) {
			defer wg.Done()
			ctx := context.Background()
			for i := range n {
				name := fmt.Sprintf("%s-%d", prefix, i)
				_, err := folder.CreateFolder(ctx, name)
				assert.NoError(t, err)
				// A VM may not share a name with a sibling folder, so the VM gets its own name.
				task, err := object.NewCommon(c.Client, ref).Rename(ctx, name+"-vm")
				if assert.NoError(t, err) {
					assert.NoError(t, task.Wait(ctx))
				}
			}
		}

		wg.Add(2)
		go run("conc-a", folderA, vmRefA, a.c)
		go run("conc-b", folderB, vmRefB, b.c)
		wg.Wait()

		for _, name := range a.names("Folder") {
			if strings.HasPrefix(name, "conc-") {
				assert.True(t, strings.HasPrefix(name, "conc-a"), "B folder %q visible in A", name)
			}
		}
		for _, name := range b.names("Folder") {
			if strings.HasPrefix(name, "conc-") {
				assert.True(t, strings.HasPrefix(name, "conc-b"), "A folder %q visible in B", name)
			}
		}
	})
}

// TestTwoESXModelsTemplateIsolation checks that the esx templates are not shared between standalone hosts.
func TestTwoESXModelsTemplateIsolation(t *testing.T) {
	a := newIsoModel(t, simulator.ESX())
	b := newIsoModel(t, simulator.ESX())

	hostName := a.names("HostSystem")[0]
	hsA := a.m.Map().Get(a.refOf("HostSystem", hostName)).(*simulator.HostSystem)
	require.NotEmpty(t, hsA.Hardware.CpuPkg)
	hostB := b.m.Map().Get(b.refOf("HostSystem", hostName)).(*simulator.HostSystem)
	before := hostB.Hardware.CpuPkg[0].Hz

	hsA.Hardware.CpuPkg[0].Hz = before + 1
	defer func() { hsA.Hardware.CpuPkg[0].Hz = before }()
	require.Equal(t, before, hostB.Hardware.CpuPkg[0].Hz)

	c := newIsoModel(t, simulator.ESX())
	hostC := c.m.Map().Get(c.refOf("HostSystem", hostName)).(*simulator.HostSystem)
	require.Equal(t, before, hostC.Hardware.CpuPkg[0].Hz)
}
