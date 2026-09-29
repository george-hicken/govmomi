// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package simulator

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"runtime/debug"
	"strings"
	"sync"

	"github.com/vmware/govmomi/toolbox"
	"github.com/vmware/govmomi/vim25/types"
)

// maxUnixSocketPath is the usable length of sun_path on linux. net.Listen
// reports a longer path only as EINVAL.
const maxUnixSocketPath = 107

// guestRPCServer serves GuestRPC (RPCI) requests from one container-backed VM
// over a Unix socket that is bind-mounted into the container.
//
// The socket carries DataMap packets only, the framing real vmtoolsd uses over
// AF_VSOCK. It carries exactly one framing because two self-delimiting framings
// on one socket cannot be told apart reliably, and the guest chooses the
// lengths.
//
// As VMX does, it lets the guest read guestinfo.* keys and write those without
// "/". Everything else is refused, including vcsim's RUN.* keys, which are
// directives for the VM's container.
type guestRPCServer struct {
	vm   *VirtualMachine
	path string // host path of the socket

	// ctx is the power-on Context. Requests are not served on it: ObjectLock
	// lets a Context re-enter a lock it holds, so a guest request would not be
	// excluded from the power-on task still running on it. Each connection
	// gets its own Context instead; see connContext.
	ctx *Context

	ln       net.Listener
	done     chan struct{}
	once     sync.Once
	accepted sync.WaitGroup
	mu       sync.Mutex
	conns    map[net.Conn]struct{}
}

func newGuestRPCServer(vm *VirtualMachine, path string) *guestRPCServer {
	return &guestRPCServer{
		vm:    vm,
		path:  path,
		done:  make(chan struct{}),
		conns: make(map[net.Conn]struct{}),
	}
}

// start listens on s.path and serves connections until stop. ctx is the
// power-on Context.
func (s *guestRPCServer) start(ctx *Context) error {
	if len(s.path) > maxUnixSocketPath {
		return fmt.Errorf("guestrpc %s: socket path %q exceeds %d bytes; set a shorter TMPDIR",
			s.vm.Name, s.path, maxUnixSocketPath)
	}

	ln, err := net.Listen("unix", s.path)
	if err != nil {
		return fmt.Errorf("guestrpc %s: %w", s.vm.Name, err)
	}
	s.ctx = ctx
	s.ln = ln

	s.accepted.Add(1)
	go s.acceptLoop()

	return nil
}

// stop closes the listener and every open connection and removes the socket
// file. It is safe to call more than once.
//
// It does not wait for requests in progress. The caller is usually a power
// task holding the VM's lock, which such a request may be waiting for; the
// request completes once the lock is released, and its reply is discarded.
func (s *guestRPCServer) stop() {
	s.once.Do(func() {
		close(s.done)
		if s.ln != nil {
			_ = s.ln.Close()
		}

		s.mu.Lock()
		for conn := range s.conns {
			_ = conn.Close()
		}
		s.mu.Unlock()
	})
	s.accepted.Wait()
	_ = os.Remove(s.path)
}

func (s *guestRPCServer) acceptLoop() {
	defer s.accepted.Done()

	for {
		conn, err := s.ln.Accept()
		if err != nil {
			select {
			case <-s.done:
			default:
				log.Printf("guestrpc %s: accept: %v", s.vm.Name, err)
			}
			return
		}

		if !s.track(conn) {
			_ = conn.Close()
			return
		}

		go s.serveConn(conn)
	}
}

// track records conn so that stop can close it, and reports false if stop has
// already begun.
func (s *guestRPCServer) track(conn net.Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	select {
	case <-s.done:
		return false
	default:
		s.conns[conn] = struct{}{}
		return true
	}
}

// serveConn answers requests on conn until the peer sets FAST_CLOSE or the
// connection fails.
func (s *guestRPCServer) serveConn(conn net.Conn) {
	defer func() {
		s.mu.Lock()
		delete(s.conns, conn)
		s.mu.Unlock()
		_ = conn.Close()
	}()
	defer func() {
		if r := recover(); r != nil {
			log.Printf("guestrpc %s: panic: %v\n%s", s.vm.Name, r, debug.Stack())
		}
	}()

	ctx := s.connContext()

	for {
		payload, fastClose, err := toolbox.ReadDataMapPacket(conn)
		if err != nil {
			return
		}

		cmd := strings.TrimRight(string(payload), "\x00")
		if Trace {
			// The command's value is guest-supplied and often carries
			// cloud-init userdata, so only the verb and key are logged.
			verb, args, _ := strings.Cut(cmd, " ")
			key, _, _ := strings.Cut(args, " ")
			log.Printf("guestrpc %s: %s %s", s.vm.Name, verb, key)
		}

		if err := toolbox.WriteDataMapPacket(conn, []byte(s.dispatch(ctx, cmd)), false); err != nil {
			return
		}

		if fastClose {
			return
		}
	}
}

// connContext returns a Context for serving one connection, distinct from the
// power-on Context and from every other connection's, so that ObjectLock
// excludes each from the others.
func (s *guestRPCServer) connContext() *Context {
	return &Context{
		Context: context.Background(),
		Session: s.ctx.Session,
		Map:     s.ctx.Map,
	}
}

// dispatch executes one RPCI command and returns its reply.
func (s *guestRPCServer) dispatch(ctx *Context, cmd string) string {
	switch {
	case strings.HasPrefix(cmd, "info-get "):
		return s.infoGet(ctx, strings.TrimPrefix(cmd, "info-get "))

	case strings.HasPrefix(cmd, "info-set "):
		key, value, ok := strings.Cut(strings.TrimPrefix(cmd, "info-set "), " ")
		if !ok {
			return "0 Bad syntax for info-set"
		}
		return s.infoSet(ctx, key, value)

	case strings.HasPrefix(cmd, "tools.capability."),
		strings.HasPrefix(cmd, "tools.set-state"),
		strings.HasPrefix(cmd, "tools.set.version"),
		strings.HasPrefix(cmd, "Capabilities_Register"):
		return "1 "

	default:
		return "0 Unknown command"
	}
}

func (s *guestRPCServer) infoGet(ctx *Context, key string) string {
	if !isGuestReadable(key) {
		return "0 Permission denied"
	}

	var (
		value string
		found bool
	)
	ctx.WithLock(s.vm, func() {
		for _, opt := range s.vm.Config.ExtraConfig {
			if v := opt.GetOptionValue(); v.Key == key {
				if v.Value != nil {
					value = fmt.Sprint(v.Value)
				}
				found = true
				return
			}
		}
	})

	if !found {
		return "0 No value found"
	}
	return "1 " + value
}

// infoSet writes key into ExtraConfig through AutoUpdate, so PropertyCollector
// reports the change.
func (s *guestRPCServer) infoSet(ctx *Context, key, value string) string {
	if !isGuestWritable(key) {
		return "0 Permission denied"
	}

	ctx.AutoUpdate(s.vm, func() {
		for _, opt := range s.vm.Config.ExtraConfig {
			if v := opt.GetOptionValue(); v.Key == key {
				v.Value = value
				return
			}
		}
		s.vm.Config.ExtraConfig = append(s.vm.Config.ExtraConfig,
			&types.OptionValue{Key: key, Value: value})
	})
	return "1 "
}

// isGuestReadable reports whether a guest may info-get key.
func isGuestReadable(key string) bool {
	return strings.HasPrefix(key, "guestinfo.")
}

// isGuestWritable reports whether a guest may info-set key. VMX makes a key
// containing "/" read-only to the guest.
func isGuestWritable(key string) bool {
	return isGuestReadable(key) && !strings.Contains(key, "/")
}
