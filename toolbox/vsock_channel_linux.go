// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package toolbox

import (
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"
	"unsafe"
)

const (
	afVsock       = 40  // AF_VSOCK
	vmaddrCIDHost = 2   // VMADDR_CID_HOST, the hypervisor
	vsockRPCIPort = 976 // GuestRPC port
)

// sockaddrVM mirrors struct sockaddr_vm from <linux/vm_sockets.h>. The syscall
// package has no AF_VSOCK address type, so connect is issued directly.
type sockaddrVM struct {
	family    uint16
	reserved1 uint16
	port      uint32
	cid       uint32
	flags     uint8
	pad       [3]uint8
}

// vsockChannel implements Channel over AF_VSOCK, speaking DataMap packets the
// way real vmtoolsd does: one connection per exchange, with FAST_CLOSE set.
//
// Start connects, so that a caller choosing among transports learns at Start
// whether anything answers. Send uses that connection for the first exchange
// and a fresh one for each later exchange; Receive closes it.
//
// A vsockChannel must not be used from more than one goroutine at a time.
type vsockChannel struct {
	cid  uint32
	port uint32
	conn net.Conn
}

// NewVsockChannelOut returns an RPCI Channel that connects over AF_VSOCK to
// the hypervisor's GuestRPC port.
func NewVsockChannelOut() Channel {
	return &vsockChannel{cid: vmaddrCIDHost, port: vsockRPCIPort}
}

func (c *vsockChannel) Start() error {
	conn, err := dialVsock(c.cid, c.port)
	if err != nil {
		return err
	}
	c.conn = conn
	return nil
}

func (c *vsockChannel) Stop() error {
	if c.conn == nil {
		return nil
	}
	err := c.conn.Close()
	c.conn = nil
	return err
}

func (c *vsockChannel) Send(buf []byte) error {
	if c.conn == nil {
		conn, err := dialVsock(c.cid, c.port)
		if err != nil {
			return err
		}
		c.conn = conn
	}
	return WriteDataMapPacket(c.conn, buf, true)
}

func (c *vsockChannel) Receive() ([]byte, error) {
	if c.conn == nil {
		return nil, errors.New("vsock channel: Receive without Send")
	}
	defer func() { _ = c.Stop() }()

	payload, _, err := ReadDataMapPacket(c.conn)
	return payload, err
}

func dialVsock(cid, port uint32) (net.Conn, error) {
	fd, err := syscall.Socket(afVsock, syscall.SOCK_STREAM, 0)
	if err != nil {
		return nil, fmt.Errorf("vsock channel: socket: %w", err)
	}

	addr := sockaddrVM{family: afVsock, port: port, cid: cid}
	_, _, errno := syscall.Syscall(
		syscall.SYS_CONNECT,
		uintptr(fd),
		uintptr(unsafe.Pointer(&addr)),
		unsafe.Sizeof(addr),
	)
	if errno != 0 {
		_ = syscall.Close(fd)
		return nil, fmt.Errorf("vsock channel: connect cid=%d port=%d: %w", cid, port, errno)
	}

	f := os.NewFile(uintptr(fd), fmt.Sprintf("vsock-cid%d-port%d", cid, port))
	conn, err := net.FileConn(f)
	_ = f.Close() // FileConn holds its own duplicate of fd
	if err != nil {
		return nil, fmt.Errorf("vsock channel: %w", err)
	}
	return conn, nil
}
