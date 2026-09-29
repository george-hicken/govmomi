// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package toolbox

import (
	"fmt"
	"net"
)

const (
	// UnixSocketPath is where a guest finds the GuestRPC Unix socket.
	// govmomi/simulator mounts it here in container-backed VMs with RUN.vmci=true.
	UnixSocketPath = "/run/vmware/rpc.sock"

	// UnixSocketEnv names the environment variable that, when set, gives the
	// GuestRPC Unix socket path in preference to UnixSocketPath.
	UnixSocketEnv = "VMX_RPC_SOCK"
)

// unixChannel implements Channel over a Unix stream socket, speaking DataMap
// packets: the framing real vmtoolsd uses over AF_VSOCK, so a server needs only
// one parser for every GuestRPC transport.
//
// The connection is opened by Start and reused for every exchange, so FAST_CLOSE
// is never set and the server keeps serving the connection.
type unixChannel struct {
	path string
	conn net.Conn
}

// NewUnixChannelOut returns an RPCI Channel that connects to the Unix socket
// at path. The host side is govmomi/simulator's GuestRPC server.
func NewUnixChannelOut(path string) Channel {
	return &unixChannel{path: path}
}

func (c *unixChannel) Start() error {
	conn, err := net.Dial("unix", c.path)
	if err != nil {
		return fmt.Errorf("unix channel: %w", err)
	}
	c.conn = conn
	return nil
}

func (c *unixChannel) Stop() error {
	if c.conn == nil {
		return nil
	}
	err := c.conn.Close()
	c.conn = nil
	return err
}

func (c *unixChannel) Send(buf []byte) error {
	return WriteDataMapPacket(c.conn, buf, false)
}

func (c *unixChannel) Receive() ([]byte, error) {
	payload, _, err := ReadDataMapPacket(c.conn)
	return payload, err
}
