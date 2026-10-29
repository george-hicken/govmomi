// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package simulator

import (
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"testing"

	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/vim25/soap"
	"github.com/vmware/govmomi/vim25/types"
)

func newTestModel(t *testing.T) (*Context, []*HostSystem) {
	t.Helper()
	m := VPX()
	m.Host = 2
	if err := m.Create(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Remove)

	ctx := m.Service.Context
	var hosts []*HostSystem
	for _, obj := range ctx.Map.AllReference("HostSystem") {
		hosts = append(hosts, obj.(*HostSystem))
	}
	return ctx, hosts
}

func endpointFor(t *testing.T, ctx *Context, h *HostSystem) Endpoint {
	t.Helper()
	e, err := ctx.Map.Endpoints.For(ctx, h)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestEndpointLoopbackAndThumbprint(t *testing.T) {
	ctx, hosts := newTestModel(t)
	ctx.Map.Endpoints.Handle(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, EndpointHost(r).Value)
	}), nil)

	for _, h := range hosts {
		e := endpointFor(t, ctx, h)

		host, _, err := net.SplitHostPort(e.Listen)
		if err != nil {
			t.Fatal(err)
		}
		if !net.ParseIP(host).IsLoopback() {
			t.Errorf("listen %s is not loopback", e.Listen)
		}
		if e.Advertised != e.Listen {
			t.Errorf("advertised %s, listen %s", e.Advertised, e.Listen)
		}

		var reported object.HostCertificateInfo
		if _, err = reported.FromPEM(h.Config.Certificate); err != nil {
			t.Fatal(err)
		}

		client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
		res, err := client.Get("https://" + e.Advertised + "/")
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(res.Body)
		_ = res.Body.Close()
		client.CloseIdleConnections()

		served := res.TLS.PeerCertificates[0]
		if soap.ThumbprintSHA1(served) != reported.ThumbprintSHA1 || soap.ThumbprintSHA256(served) != reported.ThumbprintSHA256 {
			t.Errorf("%s: served certificate does not match the reported one", h.Name)
		}
		if string(body) != h.Self.Value {
			t.Errorf("handler saw host %q, want %q", body, h.Self.Value)
		}
	}

	a, b := endpointFor(t, ctx, hosts[0]), endpointFor(t, ctx, hosts[1])
	if a.Listen == b.Listen || soap.ThumbprintSHA256(a.Certificate) == soap.ThumbprintSHA256(b.Certificate) {
		t.Error("hosts share an endpoint or key pair")
	}
}

func TestEndpointConcurrentFor(t *testing.T) {
	ctx, hosts := newTestModel(t)

	var wg sync.WaitGroup
	got := make([]Endpoint, 16)
	for i := range got {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := *ctx // one Context per caller, as per request
			got[i], _ = ctx.Map.Endpoints.For(&c, hosts[i%2])
		}()
	}
	wg.Wait()

	for i, e := range got {
		if e.Listen == "" || e.Listen != got[i%2].Listen {
			t.Fatalf("call %d: endpoint %q, want %q", i, e.Listen, got[i%2].Listen)
		}
	}
}

func TestEndpointStop(t *testing.T) {
	ctx, hosts := newTestModel(t)
	e := endpointFor(t, ctx, hosts[0])

	ctx.Map.Endpoints.Stop(hosts[0].Self)
	if c, err := net.Dial("tcp", e.Listen); err == nil {
		_ = c.Close()
		t.Fatal("endpoint still listening after Stop")
	}
	if e2 := endpointFor(t, ctx, hosts[0]); e2.Listen == e.Listen {
		t.Error("restarted endpoint reused the stopped one")
	}
}

// TestInstanceIsolation covers 931.h: two instances in one process share no ports or tickets.
func TestInstanceIsolation(t *testing.T) {
	ctxA, hostsA := newTestModel(t)
	ctxB, hostsB := newTestModel(t)

	seen := map[string]bool{}
	for _, p := range []struct {
		ctx   *Context
		hosts []*HostSystem
	}{{ctxA, hostsA}, {ctxB, hostsB}} {
		for _, h := range p.hosts {
			e := endpointFor(t, p.ctx, h)
			if seen[e.Listen] {
				t.Fatalf("port collision on %s", e.Listen)
			}
			seen[e.Listen] = true
		}
	}

	b := TicketBinding{VM: types.ManagedObjectReference{Type: "VirtualMachine", Value: "vm-1"}, Host: hostsA[0].Self}
	s, err := ctxA.Map.Tickets.Issue(b)
	if err != nil {
		t.Fatal(err)
	}
	if err = ctxB.Map.Tickets.Redeem(s, b); !errors.Is(err, ErrTicketUnknown) {
		t.Fatalf("instance B redeemed A's ticket: %v", err)
	}
}
