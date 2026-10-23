// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package toolbox

// noopChannel is a Channel on which nothing ever arrives.
type noopChannel struct{}

// NewNoopChannelIn returns a TCLO Channel on which nothing ever arrives, for a
// transport with no host-to-guest direction. A Service given it stays running,
// polling at its maximum delay, and handles only what the guest initiates.
func NewNoopChannelIn() Channel {
	return &noopChannel{}
}

func (c *noopChannel) Start() error             { return nil }
func (c *noopChannel) Stop() error              { return nil }
func (c *noopChannel) Send(_ []byte) error      { return nil }
func (c *noopChannel) Receive() ([]byte, error) { return nil, nil }
