// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package simulator

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/vmware/govmomi"
	"github.com/vmware/govmomi/find"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/property"
	"github.com/vmware/govmomi/toolbox"
	"github.com/vmware/govmomi/vim25/types"
)

// vcSimEnv is a running VPX model with a client connected to it.
type vcSimEnv struct {
	goCtx  context.Context
	simCtx *Context
	client *govmomi.Client
	pool   *object.ResourcePool
	folder *object.DatacenterFolders
	pc     *property.Collector
}

// newVCSIMEnv starts a VPX model and connects a client, both released by
// t.Cleanup.
func newVCSIMEnv(t *testing.T) *vcSimEnv {
	t.Helper()

	goCtx := context.Background()
	m := VPX()
	t.Cleanup(m.Remove)
	require.NoError(t, m.Create())
	s := m.Service.NewServer()
	t.Cleanup(s.Close)

	c, err := govmomi.NewClient(goCtx, s.URL, true)
	require.NoError(t, err)

	finder := find.NewFinder(c.Client)
	pool, err := finder.ResourcePool(goCtx, "DC0_H0/Resources")
	require.NoError(t, err)
	dc, err := finder.Datacenter(goCtx, "DC0")
	require.NoError(t, err)
	f, err := dc.Folders(goCtx)
	require.NoError(t, err)

	return &vcSimEnv{
		goCtx:  goCtx,
		simCtx: m.Service.Context,
		client: c,
		pool:   pool,
		folder: f,
		pc:     property.DefaultCollector(c.Client),
	}
}

// waitExtraConfig waits, through PropertyCollector, for vmRef's
// config.extraConfig to hold key=want, failing the test after timeout.
func waitExtraConfig(t *testing.T, env *vcSimEnv, vmRef types.ManagedObjectReference, key, want string, timeout time.Duration) {
	t.Helper()

	ctx, cancel := context.WithTimeout(env.goCtx, timeout)
	defer cancel()

	var got any
	err := property.Wait(ctx, env.pc, vmRef, []string{"config.extraConfig"}, func(changes []types.PropertyChange) bool {
		for _, change := range changes {
			options, _ := change.Val.(types.ArrayOfOptionValue)
			for _, opt := range options.OptionValue {
				if v := opt.GetOptionValue(); v.Key == key {
					got = v.Value
				}
			}
		}
		return got == want
	})
	require.NoError(t, err, "%s: got %v, want %q within %s", key, got, want, timeout)
}

// serveGuestRPC serves GuestRPC for vm, as RUN.vmci does, and returns a
// connected client. Both are released by t.Cleanup.
func serveGuestRPC(t *testing.T, env *vcSimEnv, vm *VirtualMachine) (*guestRPCServer, *toolbox.ChannelOut) {
	t.Helper()

	srv := newGuestRPCServer(vm, filepath.Join(t.TempDir(), "rpc.sock"))
	require.NoError(t, srv.start(env.simCtx))
	t.Cleanup(srv.stop)

	return srv, dialGuestRPC(t, srv.path)
}

// dialGuestRPC returns a client connected to the socket at path, closed by
// t.Cleanup.
func dialGuestRPC(t *testing.T, path string) *toolbox.ChannelOut {
	t.Helper()

	ch := toolbox.NewUnixChannelOut(path)
	require.NoError(t, ch.Start())
	t.Cleanup(func() { _ = ch.Stop() })

	return &toolbox.ChannelOut{Channel: ch}
}

// allVMs returns every VirtualMachine in the model.
func allVMs(t *testing.T, env *vcSimEnv) []*VirtualMachine {
	t.Helper()

	var vms []*VirtualMachine
	for _, ref := range env.simCtx.Map.AllReference("VirtualMachine") {
		vms = append(vms, env.simCtx.Map.Get(ref.Reference()).(*VirtualMachine))
	}
	require.NotEmpty(t, vms)

	return vms
}

func TestGuestRPCServer_InfoGetSet(t *testing.T) {
	env := newVCSIMEnv(t)
	_, out := serveGuestRPC(t, env, allVMs(t, env)[0])

	_, err := out.Request([]byte("info-set guestinfo.test.key hello world"))
	require.NoError(t, err)

	reply, err := out.Request([]byte("info-get guestinfo.test.key"))
	require.NoError(t, err)
	require.Equal(t, "hello world", string(reply))

	_, err = out.Request([]byte("info-get guestinfo.test.missing"))
	require.ErrorContains(t, err, "No value found")
}

// TestGuestRPCServer_InfoSet_PropertyChange pins that a guest write reaches a
// PropertyCollector client within a second.
func TestGuestRPCServer_InfoSet_PropertyChange(t *testing.T) {
	env := newVCSIMEnv(t)
	vm := allVMs(t, env)[0]
	_, out := serveGuestRPC(t, env, vm)

	_, err := out.Request([]byte("info-set guestinfo.notify.test triggered"))
	require.NoError(t, err)

	waitExtraConfig(t, env, vm.Self, "guestinfo.notify.test", "triggered", time.Second)
}

// TestGuestRPCServer_HostWriteGuestRead pins that the guest reads the live
// ExtraConfig, not a copy taken when the server started.
func TestGuestRPCServer_HostWriteGuestRead(t *testing.T) {
	env := newVCSIMEnv(t)
	vm := allVMs(t, env)[0]
	_, out := serveGuestRPC(t, env, vm)

	task, err := object.NewVirtualMachine(env.client.Client, vm.Self).Reconfigure(env.goCtx, types.VirtualMachineConfigSpec{
		ExtraConfig: []types.BaseOptionValue{&types.OptionValue{Key: "guestinfo.from.host", Value: "hostvalue"}},
	})
	require.NoError(t, err)
	require.NoError(t, task.Wait(env.goCtx))

	reply, err := out.Request([]byte("info-get guestinfo.from.host"))
	require.NoError(t, err)
	require.Equal(t, "hostvalue", string(reply))
}

// TestGuestRPCServer_MultiVM pins that each VM's server reaches only its own
// VM's ExtraConfig.
func TestGuestRPCServer_MultiVM(t *testing.T) {
	env := newVCSIMEnv(t)
	vms := allVMs(t, env)
	require.GreaterOrEqual(t, len(vms), 2)

	var outs []*toolbox.ChannelOut
	for i, vm := range vms[:2] {
		_, out := serveGuestRPC(t, env, vm)
		outs = append(outs, out)

		_, err := out.Request([]byte(fmt.Sprintf("info-set guestinfo.multivm.vm%d vm%d-value", i, i)))
		require.NoError(t, err)
	}

	for i, out := range outs {
		reply, err := out.Request([]byte(fmt.Sprintf("info-get guestinfo.multivm.vm%d", i)))
		require.NoError(t, err)
		require.Equal(t, fmt.Sprintf("vm%d-value", i), string(reply))

		_, err = out.Request([]byte(fmt.Sprintf("info-get guestinfo.multivm.vm%d", 1-i)))
		require.Error(t, err, "VM %d must not see VM %d's key", i, 1-i)
	}
}

// TestGuestRPCServer_StopAndRestart pins that stop releases a connection the
// guest still holds, removes the socket, and leaves the path free for the
// next power-on.
func TestGuestRPCServer_StopAndRestart(t *testing.T) {
	env := newVCSIMEnv(t)
	vm := allVMs(t, env)[0]
	srv, out := serveGuestRPC(t, env, vm)

	_, err := out.Request([]byte("info-set guestinfo.restart.test before"))
	require.NoError(t, err)

	stopped := make(chan struct{})
	go func() {
		srv.stop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("stop blocked on an open guest connection")
	}

	_, err = os.Stat(srv.path)
	require.True(t, os.IsNotExist(err), "socket must be removed by stop: %v", err)
	_, err = out.Request([]byte("info-get guestinfo.restart.test"))
	require.Error(t, err, "a connection must not outlive stop")

	restarted := newGuestRPCServer(vm, srv.path)
	require.NoError(t, restarted.start(env.simCtx))
	t.Cleanup(restarted.stop)

	reply, err := dialGuestRPC(t, srv.path).Request([]byte("info-get guestinfo.restart.test"))
	require.NoError(t, err)
	require.Equal(t, "before", strings.TrimSpace(string(reply)))
}
