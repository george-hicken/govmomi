// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package simulator

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// eventsStub stands in for the docker CLI. Like the podman shim, it forks a child that outlives a direct kill of the
// parent, and records the child's pid for the test.
const eventsStub = `#!/bin/sh
sleep 300 &
echo $! > "$VCSIM_TEST_CHILD_PID"
wait
`

// TestEventWatcherStopsWithModel checks that removing a model kills the docker events watcher it started, including
// the child the docker CLI forked.
func TestEventWatcherStopsWithModel(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(eventsStub), 0o700); err != nil {
		t.Fatal(err)
	}
	childFile := filepath.Join(dir, "child.pid")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("VCSIM_TEST_CHILD_PID", childFile)

	t.Cleanup(func() {
		// Keep the stub's child from outliving a failed test.
		if child, err := readChildPID(childFile); err == nil && alive(child) {
			_ = syscall.Kill(child, syscall.SIGKILL)
		}
	})

	m := ESX()
	defer m.Remove()
	if err := m.Create(); err != nil {
		t.Fatal(err)
	}

	w := &m.Map().eventWatch
	w.watch(&container{id: "vcsim-event-watcher-test"})

	w.Lock()
	if w.cmd == nil {
		w.Unlock()
		t.Fatal("docker events was not started")
	}
	w.Unlock()

	child := waitForChildPID(t, childFile)
	if !alive(child) {
		t.Fatalf("docker events child %d is not running before the model is removed", child)
	}

	m.Remove()

	deadline := time.Now().Add(10 * time.Second)
	for alive(child) {
		if time.Now().After(deadline) {
			t.Fatalf("docker events child %d still running after the model was removed", child)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// waitForChildPID polls for the pid the stub writes, and fails the test if it does not appear.
func waitForChildPID(t *testing.T, file string) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		child, err := readChildPID(file)
		if err == nil {
			return child
		}
		if time.Now().After(deadline) {
			t.Fatalf("stub docker did not record its child: %s", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func readChildPID(file string) (int, error) {
	b, err := os.ReadFile(file)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(b)))
}

// alive reports whether pid is running. A zombie is not running, and is treated as gone.
func alive(pid int) bool {
	if err := syscall.Kill(pid, 0); err != nil {
		return false
	}
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return true
	}
	i := bytes.LastIndexByte(b, ')')
	return i < 0 || i+2 >= len(b) || b[i+2] != 'Z'
}
