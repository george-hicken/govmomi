// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

//go:build !linux

package toolbox

import "errors"

var errVsockUnsupported = errors.New("vsock channel: AF_VSOCK is only supported on linux")

// vsockChannel is the non-linux stand-in: every operation that would touch
// the socket fails, so a caller choosing among transports moves on at Start.
type vsockChannel struct{}

// NewVsockChannelOut returns an RPCI Channel whose Start always fails on this
// platform.
func NewVsockChannelOut() Channel {
	return &vsockChannel{}
}

func (c *vsockChannel) Start() error             { return errVsockUnsupported }
func (c *vsockChannel) Stop() error              { return nil }
func (c *vsockChannel) Send(_ []byte) error      { return errVsockUnsupported }
func (c *vsockChannel) Receive() ([]byte, error) { return nil, errVsockUnsupported }
