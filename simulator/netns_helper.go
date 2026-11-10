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
	// netnsHelperEnv, when set, makes a process act as a netns helper on the socket it names. The suffix keeps
	// the name from being set by accident, since any binary that imports this package checks it at start-up.
	netnsHelperEnv = "VCSIM_NETNS_HELPER_SOCKET_7f3a9c21"

	// netnsOwnerEnv carries the PID of the process that owns the helper. The helper exits when that process does,
	// so a crashed run does not leave a helper holding a router namespace open.
	netnsOwnerEnv = "VCSIM_NETNS_OWNER_7f3a9c21"

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
	// Errno and Timeout carry the failure's kind across the socket, so the host side returns the
	// error a direct dial or listen would: an *net.OpError over the syscall errno, or a timeout.
	Errno   int  `json:"errno,omitempty"`
	Timeout bool `json:"timeout,omitempty"`
}

// setErr records err in resp: its text, and its errno or timeout when it has one.
func (resp *netnsResponse) setErr(err error) {
	if err == nil {
		return
	}
	resp.Error = err.Error()
	var errno syscall.Errno
	if errors.As(err, &errno) {
		resp.Errno = int(errno)
	}
	var ne net.Error
	resp.Timeout = errors.As(err, &ne) && ne.Timeout()
}

// err is the error resp describes, as a direct op would return it, or nil.
func (resp netnsResponse) err(op, addr string) error {
	if resp.Error == "" {
		return nil
	}
	sysOp := map[string]string{"dial": "connect", "listen": "bind"}[op]
	switch {
	case resp.Errno != 0 && sysOp != "":
		return &net.OpError{Op: op, Net: "tcp", Addr: netnsAddr(addr), Err: os.NewSyscallError(sysOp, syscall.Errno(resp.Errno))}
	case resp.Timeout:
		return &net.OpError{Op: op, Net: "tcp", Addr: netnsAddr(addr), Err: netnsTimeout(resp.Error)}
	default:
		return errors.New(resp.Error)
	}
}

// netnsAddr is a net.Addr naming a request's address, for the rebuilt *net.OpError.
type netnsAddr string

func (a netnsAddr) Network() string { return "tcp" }
func (a netnsAddr) String() string  { return string(a) }

// netnsTimeout is a timeout reported by the helper; it satisfies net.Error.
type netnsTimeout string

func (t netnsTimeout) Error() string   { return string(t) }
func (t netnsTimeout) Timeout() bool   { return true }
func (t netnsTimeout) Temporary() bool { return true }

// init makes any binary that imports this package a netns helper when the container runtime re-executes it as
// one, before main or TestMain runs. Without the environment variable it does nothing.
func init() {
	sock := os.Getenv(netnsHelperEnv)
	if sock == "" {
		return
	}
	owner, _ := strconv.Atoi(os.Getenv(netnsOwnerEnv))
	if err := ServeNetnsHelper(sock, owner); err != nil {
		fmt.Fprintln(os.Stderr, "netns helper:", err)
		os.Exit(1)
	}
	os.Exit(0)
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
		var err error
		f, err = listenFile(req.Addr)
		resp.setErr(err)
	case "dial":
		var err error
		f, err = dialFile(req.Addr)
		resp.setErr(err)
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
func listenFile(addr string) (*os.File, error) {
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	defer func() { _ = l.Close() }()

	return l.(*net.TCPListener).File()
}

// dialFile connects to addr from the helper's namespace and returns a dup of the connection's descriptor.
func dialFile(addr string) (*os.File, error) {
	conn, err := net.DialTimeout("tcp", addr, netnsDialTimeout)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()

	return conn.(*net.TCPConn).File()
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
	if err := resp.err(req.Op, req.Addr); err != nil {
		return nil, fmt.Errorf("netns helper %s %s: %w", req.Op, req.Addr, err)
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
