// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

// Command toolbox is a guest agent built on the govmomi/toolbox package. It can
// be run on a VM hosted by ESX, Fusion or Workstation, and in a
// govmomi/simulator container-backed VM with RUN.vmci=true.
//
// The mode is chosen by how it is invoked:
//
//	toolbox                       daemon: serve TCLO requests from the host
//	toolbox --cmd 'COMMAND'       send one RPCI command, as vmtoolsd --cmd
//	vmware-rpctool 'COMMAND'      send one RPCI command, as vmware-rpctool
//
// The two one-shot modes reproduce the output of the open-vm-tools commands,
// which guest software parses: a "1 " reply is printed without that prefix on
// stdout with exit status 0, and a "0 " reply without its prefix on stderr
// with exit status 1. When no transport is reachable each prints what its
// open-vm-tools counterpart prints outside a VM.
//
// Every mode tries the same transports, in this order, and uses the first that
// accepts a connection:
//
//  1. the Unix socket named by $VMX_RPC_SOCK
//  2. the Unix socket at /run/vmware/rpc.sock, for processes such as systemd
//     units that do not inherit the environment the socket was announced in
//  3. the x86 backdoor
//  4. AF_VSOCK to the hypervisor
//
// Over a Unix socket or vsock no host-to-guest (TCLO) requests arrive, so the
// daemon only keeps running.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/vmware/govmomi/toolbox"
)

func main() {
	if strings.Contains(filepath.Base(os.Args[0]), "vmware-rpctool") {
		os.Exit(rpctool(os.Args))
	}

	cmd := flag.String("cmd", "", "send one RPCI command and exit, as vmtoolsd --cmd")
	flag.Parse()

	if *cmd != "" {
		os.Exit(vmtoolsdCmd(*cmd))
	}

	in, out := daemonChannels()
	service := toolbox.NewService(in, out)
	service.GuestInfoInterval = toolbox.DefaultGuestInfoInterval
	service.ReportVersion = true

	if os.Getuid() == 0 {
		service.Power.Halt.Handler = toolbox.Halt
		service.Power.Reboot.Handler = toolbox.Reboot
	}

	if err := service.Start(); err != nil {
		log.Fatal(err)
	}

	// handle the signals and gracefully shutdown the service
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		log.Printf("signal %s received", <-sig)
		service.Stop()
	}()

	service.Wait()
}

const rpctoolUsage = "rpctool syntax:\n\n" +
	"  -h | --help\tprint usage.\n" +
	"  rpctool <text>\tsend <text> as an RPC command.\n"

// rpctool behaves as open-vm-tools rpctool/rpctool.c and returns the exit
// status.
func rpctool(args []string) int {
	ch := openRPCChannel()
	if ch == nil {
		fmt.Fprintf(os.Stderr, "Error: %s must be run inside a virtual machine"+
			" on a VMware hypervisor product.\n", args[0])
		return 255
	}
	defer ch.Stop()

	if len(args) <= 1 {
		fmt.Fprint(os.Stderr, rpctoolUsage)
		return 1
	}
	if strings.HasPrefix(args[1], "-h") || strings.HasPrefix(args[1], "--help") {
		fmt.Fprint(os.Stderr, rpctoolUsage)
		return 0
	}

	return runRPC(ch, args[1])
}

// vmtoolsdCmd behaves as vmtoolsd --cmd (open-vm-tools
// services/vmtoolsd/cmdLine.c) and returns the exit status.
func vmtoolsdCmd(command string) int {
	ch := openRPCChannel()
	if ch == nil {
		fmt.Fprintln(os.Stderr, "Unable to send command to VMware hypervisor.")
		return 1
	}
	defer ch.Stop()

	return runRPC(ch, command)
}

// runRPC sends command, prints the reply as both one-shot commands do, and
// returns the exit status.
func runRPC(ch toolbox.Channel, command string) int {
	reply, ok := sendOne(ch, command)
	if !ok {
		fmt.Fprintln(os.Stderr, reply)
		return 1
	}
	fmt.Println(reply)
	return 0
}

// sendOne sends command as open-vm-tools' RpcOut_sendOne does. It returns the
// reply without its status prefix and whether the command succeeded, or, on a
// channel failure, the message RpcOut_send gives for it.
func sendOne(ch toolbox.Channel, command string) (string, bool) {
	// Old VMXs expect a space in every command, so open-vm-tools adds one to a
	// command without arguments.
	if !strings.Contains(command, " ") {
		command += " "
	}

	if err := ch.Send([]byte(command)); err != nil {
		return "RpcOut: Unable to send the RPCI command", false
	}
	reply, err := ch.Receive()
	if err != nil {
		return "RpcOut: Unable to receive the result of the RPCI command", false
	}

	reply = bytes.TrimRight(reply, "\x00")
	switch {
	case bytes.HasPrefix(reply, []byte("1 ")):
		return string(reply[2:]), true
	case bytes.HasPrefix(reply, []byte("0 ")):
		return string(reply[2:]), false
	default:
		return "RpcOut: Invalid format for the result of the RPCI command", false
	}
}

// daemonChannels returns the TCLO and RPCI channels of the first transport
// that accepts a connection. The probe is discarded, and unopened channels are
// returned, because Service.Start opens the channels it is given.
func daemonChannels() (in, out toolbox.Channel) {
	transports := rpcTransports()

	for _, t := range transports {
		probe := t.out()
		if probe.Start() == nil {
			_ = probe.Stop()
			return t.in(), t.out()
		}
	}

	// Nothing answered: hand over the last transport so that Service.Start
	// reports the channel error.
	last := transports[len(transports)-1]
	return last.in(), last.out()
}

// openRPCChannel returns a started RPCI channel on the first transport that
// accepts a connection, or nil if none does.
func openRPCChannel() toolbox.Channel {
	for _, t := range rpcTransports() {
		ch := t.out()
		if ch.Start() == nil {
			return ch
		}
	}
	return nil
}

// rpcTransport is one GuestRPC transport. Its channels are constructors, so a
// transport can be probed and then handed, unopened, to a Service.
type rpcTransport struct {
	in  func() toolbox.Channel
	out func() toolbox.Channel
}

// rpcTransports returns the transports in the order documented for the
// command. A transport is chosen by connecting to it: that the kernel can
// create an AF_VSOCK socket, for instance, says nothing about whether
// anything answers on it.
func rpcTransports() []rpcTransport {
	paths := []string{toolbox.UnixSocketPath}
	if env := os.Getenv(toolbox.UnixSocketEnv); env != "" && env != toolbox.UnixSocketPath {
		paths = []string{env, toolbox.UnixSocketPath}
	}

	var transports []rpcTransport
	for _, path := range paths {
		transports = append(transports, rpcTransport{
			in:  toolbox.NewNoopChannelIn,
			out: func() toolbox.Channel { return toolbox.NewUnixChannelOut(path) },
		})
	}

	return append(transports,
		rpcTransport{
			in:  toolbox.NewBackdoorChannelIn,
			out: toolbox.NewBackdoorChannelOut,
		},
		rpcTransport{
			in:  toolbox.NewNoopChannelIn,
			out: toolbox.NewVsockChannelOut,
		},
	)
}
