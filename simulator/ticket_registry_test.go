// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package simulator

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vmware/govmomi/vim25/types"
)

var testBinding = TicketBinding{
	VM:    types.ManagedObjectReference{Type: "VirtualMachine", Value: "vm-1"},
	Param: "1024",
	Host:  types.ManagedObjectReference{Type: "HostSystem", Value: "host-1"},
}

func issue(t *testing.T, r *TicketRegistry, b TicketBinding) string {
	t.Helper()
	s, err := r.Issue(b)
	if err != nil {
		t.Fatal(err)
	}
	if len(s) < 32 {
		t.Fatalf("ticket %q has fewer than 128 bits", s)
	}
	return s
}

func TestTicketSingleUse(t *testing.T) {
	r := new(TicketRegistry)
	s := issue(t, r, testBinding)

	if err := r.Redeem(s, testBinding); err != nil {
		t.Fatal(err)
	}
	if err := r.Redeem(s, testBinding); !errors.Is(err, ErrTicketUnknown) {
		t.Fatalf("second redeem: %v", err)
	}
}

func TestTicketTTL(t *testing.T) {
	now := time.Now()
	r := &TicketRegistry{Now: func() time.Time { return now }}
	s := issue(t, r, testBinding)

	now = now.Add(DefaultTicketTTL)
	if err := r.Redeem(s, testBinding); !errors.Is(err, ErrTicketExpired) {
		t.Fatalf("redeem at TTL: %v", err)
	}

	r.TTL = time.Minute
	s = issue(t, r, testBinding)
	now = now.Add(59 * time.Second)
	if err := r.Redeem(s, testBinding); err != nil {
		t.Fatalf("redeem within configured TTL: %v", err)
	}
}

// TestTicketConsumedOnFailure is the negative control for D-931-01: without consume on first presentation,
// a client retrying a failed redemption with the same ticket succeeds. The TLA+ model names the same failure,
// tla/931-remote-vsock-proxy/remote_vsock_proxy_931_no_consume.cfg.
func TestTicketConsumedOnFailure(t *testing.T) {
	wrongHost := testBinding
	wrongHost.Host.Value = "host-2"
	wrongVM := testBinding
	wrongVM.VM.Value = "vm-2"
	wrongParam := testBinding
	wrongParam.Param = "1025"

	r := new(TicketRegistry)
	for _, wrong := range []TicketBinding{wrongHost, wrongVM, wrongParam} {
		s := issue(t, r, testBinding)
		if err := r.Redeem(s, wrong); !errors.Is(err, ErrTicketMismatch) {
			t.Fatalf("redeem %v: %v", wrong, err)
		}
		if err := r.Redeem(s, testBinding); !errors.Is(err, ErrTicketUnknown) {
			t.Fatalf("retry after failed redeem: %v", err)
		}
	}
}

func TestTicketConcurrentRedeem(t *testing.T) {
	r := new(TicketRegistry)
	s := issue(t, r, testBinding)

	var ok atomic.Int32
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if r.Redeem(s, testBinding) == nil {
				ok.Add(1)
			}
			_, _ = r.Issue(testBinding)
		}()
	}
	wg.Wait()

	if n := ok.Load(); n != 1 {
		t.Fatalf("%d redemptions succeeded", n)
	}
}
