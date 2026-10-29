// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package simulator

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
	"time"

	"github.com/vmware/govmomi/vim25/types"
)

// DefaultTicketTTL is the time to live of an issued ticket unless TicketRegistry.TTL is set.
const DefaultTicketTTL = 30 * time.Second

// Errors returned by TicketRegistry.Redeem.
var (
	ErrTicketUnknown  = errors.New("ticket unknown")
	ErrTicketExpired  = errors.New("ticket expired")
	ErrTicketMismatch = errors.New("ticket not valid for this VM, parameter or host")
)

// TicketBinding is what a ticket is bound to. Param is opaque to the registry, for example a port.
type TicketBinding struct {
	VM    types.ManagedObjectReference
	Param string
	Host  types.ManagedObjectReference
}

type ticket struct {
	TicketBinding
	expires time.Time
}

// TicketRegistry issues single-use, short-lived tickets for one simulator instance.
// A ticket is consumed on first presentation, whether or not the presentation succeeds.
type TicketRegistry struct {
	// TTL overrides DefaultTicketTTL when non-zero.
	TTL time.Duration
	// Now overrides time.Now, for tests.
	Now func() time.Time

	m       sync.Mutex
	tickets map[string]ticket
}

func (r *TicketRegistry) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *TicketRegistry) ttl() time.Duration {
	if r.TTL != 0 {
		return r.TTL
	}
	return DefaultTicketTTL
}

// Issue returns a new opaque ticket of 128 random bits bound to b.
func (r *TicketRegistry) Issue(b TicketBinding) (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", err
	}
	s := hex.EncodeToString(id[:])
	now := r.now()

	r.m.Lock()
	defer r.m.Unlock()

	if r.tickets == nil {
		r.tickets = make(map[string]ticket)
	}
	for k, t := range r.tickets { // expired tickets can never be redeemed
		if !now.Before(t.expires) {
			delete(r.tickets, k)
		}
	}
	r.tickets[s] = ticket{TicketBinding: b, expires: now.Add(r.ttl())}

	return s, nil
}

// Redeem consumes ticket s and reports whether it was valid for b.
func (r *TicketRegistry) Redeem(s string, b TicketBinding) error {
	r.m.Lock()
	t, ok := r.tickets[s]
	delete(r.tickets, s) // consume on first presentation (D-931-01)
	r.m.Unlock()

	switch {
	case !ok:
		return ErrTicketUnknown
	case !r.now().Before(t.expires):
		return ErrTicketExpired
	case t.TicketBinding != b:
		return ErrTicketMismatch
	}
	return nil
}
