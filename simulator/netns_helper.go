// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package simulator

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"syscall"
	"time"
)

const (
	// NetnsHelperEnv, when set, makes a process act as a netns helper on the socket it names.
	// See ServeNetnsHelperFromEnv.
	NetnsHelperEnv = "VCSIM_NETNS_HELPER"

	// netnsOwnerEnv carries the PID of the process that owns the helper. The helper exits when that process does,
	// so a crashed run does not leave a helper holding a router namespace open.
	netnsOwnerEnv = "VCSIM_NETNS_OWNER"

	netnsDialTimeout = 10 * time.Second
)

// netnsRequest asks the helper, which runs inside a router's network namespace, for a socket.
type netnsRequest struct {
	Op   string `json:"op"` // listen, dial or shutdown
	Addr string `json:"addr,omitempty"`
}

// netnsResponse precedes the file descriptor, which travels as SCM_RIGHTS ancillary data.
type netnsResponse struct {
	Error string `json:"error,omitempty"`
}

// ServeNetnsHelperFromEnv serves as a netns helper when NetnsHelperEnv is set and reports whether it did.
// Any binary that the runtime may re-execute as a helper (vcsim, test binaries) calls it before anything else.
func ServeNetnsHelperFromEnv() (bool, error) {
	sock := os.Getenv(NetnsHelperEnv)
	if sock == "" {
		return false, nil
	}
	owner, _ := strconv.Atoi(os.Getenv(netnsOwnerEnv))
	return true, ServeNetnsHelper(sock, owner)
}

// ServeNetnsHelper serves listen, dial and shutdown requests on a unix socket until it is told to shut
// down or the owning process (owner, if non-zero) exits. It must run inside the target network namespace,
// for example as `podman unshare nsenter -t <pid> -n <self>`, so that the sockets it hands back are bound there.
func ServeNetnsHelper(socket string, owner int) error {
	l, err := net.Listen("unix", socket)
	if err != nil {
		return err
	}
	defer func() { _ = l.Close() }()

	if owner > 0 {
		go func() {
			for pidAlive(owner) {
				time.Sleep(time.Second)
			}
			_ = l.Close()
		}()
	}

	for {
		c, err := l.(*net.UnixListener).AcceptUnix()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		if shutdown := serveNetnsRequest(c); shutdown {
			return nil
		}
	}
}

// serveNetnsRequest handles one request and reports whether it was a shutdown.
func serveNetnsRequest(c *net.UnixConn) (shutdown bool) {
	defer func() { _ = c.Close() }()

	var req netnsRequest
	if err := json.NewDecoder(c).Decode(&req); err != nil {
		return false
	}

	var f *os.File
	var resp netnsResponse
	switch req.Op {
	case "listen":
		f, resp.Error = listenFile(req.Addr)
	case "dial":
		f, resp.Error = dialFile(req.Addr)
	case "shutdown":
		shutdown = true
	default:
		resp.Error = "unknown op " + req.Op
	}

	if shutdown {
		_, _ = c.Write([]byte("{}"))
		return true
	}

	payload, _ := json.Marshal(resp)
	if f == nil {
		_, _ = c.Write(payload)
		return false
	}
	defer func() { _ = f.Close() }()

	_, _, _ = c.WriteMsgUnix(payload, syscall.UnixRights(int(f.Fd())), nil)
	return false
}

// listenFile binds a TCP listener in the helper's namespace and returns a dup of its descriptor.
func listenFile(addr string) (*os.File, string) {
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err.Error()
	}
	defer func() { _ = l.Close() }()

	f, err := l.(*net.TCPListener).File()
	if err != nil {
		return nil, err.Error()
	}
	return f, ""
}

// dialFile connects to addr from the helper's namespace and returns a dup of the connection's descriptor.
func dialFile(addr string) (*os.File, string) {
	conn, err := net.DialTimeout("tcp", addr, netnsDialTimeout)
	if err != nil {
		return nil, err.Error()
	}
	defer func() { _ = conn.Close() }()

	f, err := conn.(*net.TCPConn).File()
	if err != nil {
		return nil, err.Error()
	}
	return f, ""
}

// netnsHelper is the host-side client of a helper running in a router's namespace.
type netnsHelper struct {
	socket string
}

// Listen returns a listener bound to addr inside the helper's namespace. The listening socket is
// handed over as a descriptor, so the host needs no route to addr.
func (h *netnsHelper) Listen(addr string) (net.Listener, error) {
	f, err := h.request(netnsRequest{Op: "listen", Addr: addr})
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	return net.FileListener(f)
}

// Dial returns a connection made from the helper's namespace to addr.
func (h *netnsHelper) Dial(addr string) (net.Conn, error) {
	f, err := h.request(netnsRequest{Op: "dial", Addr: addr})
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	return net.FileConn(f)
}

// shutdown asks the helper to exit. The caller must wait for it before removing the router's namespace.
func (h *netnsHelper) shutdown() error {
	c, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: h.socket, Net: "unix"})
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()

	if err := json.NewEncoder(c).Encode(netnsRequest{Op: "shutdown"}); err != nil {
		return err
	}
	var resp [2]byte
	_, _ = c.Read(resp[:])
	return nil
}

func (h *netnsHelper) request(req netnsRequest) (*os.File, error) {
	c, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: h.socket, Net: "unix"})
	if err != nil {
		return nil, err
	}
	defer func() { _ = c.Close() }()

	if err := json.NewEncoder(c).Encode(req); err != nil {
		return nil, err
	}

	buf := make([]byte, 4096)
	oob := make([]byte, syscall.CmsgSpace(4))
	n, oobn, _, _, err := c.ReadMsgUnix(buf, oob)
	if err != nil {
		return nil, fmt.Errorf("netns helper %s: %w", req.Op, err)
	}

	var resp netnsResponse
	if err := json.Unmarshal(buf[:n], &resp); err != nil {
		return nil, fmt.Errorf("netns helper %s: %w", req.Op, err)
	}
	if resp.Error != "" {
		return nil, fmt.Errorf("netns helper %s %s: %s", req.Op, req.Addr, resp.Error)
	}

	msgs, err := syscall.ParseSocketControlMessage(oob[:oobn])
	if err != nil {
		return nil, fmt.Errorf("netns helper %s: %w", req.Op, err)
	}
	if len(msgs) != 1 {
		return nil, fmt.Errorf("netns helper %s: got %d control messages, want 1", req.Op, len(msgs))
	}
	fds, err := syscall.ParseUnixRights(&msgs[0])
	if err != nil || len(fds) != 1 {
		return nil, fmt.Errorf("netns helper %s: no descriptor received", req.Op)
	}

	return os.NewFile(uintptr(fds[0]), req.Op+" "+req.Addr), nil
}

// pidAlive reports whether a process with pid exists. EPERM means it exists under another user.
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
