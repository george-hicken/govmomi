// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package simulator

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/google/uuid"
)

// errRunLocked means a run's lock is held, so a live vcsim instance owns the run's objects.
var errRunLocked = errors.New("run is held by a live vcsim instance")

// runLock is an exclusive flock on a run's lock file. The kernel drops the lock when its holder exits, so a run
// whose lock can be taken has no live owner. Unlike a PID, the lock means the same thing in every PID namespace.
type runLock struct {
	once sync.Once
	f    *os.File
}

// runLockDir returns the directory of run lock files: $XDG_RUNTIME_DIR/vcsim, or a per-user directory under the
// temp dir when XDG_RUNTIME_DIR is unset.
func runLockDir() string {
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		return filepath.Join(dir, "vcsim")
	}
	return filepath.Join(os.TempDir(), "vcsim-"+strconv.Itoa(os.Getuid()))
}

// acquireRunLock takes run's lock without blocking. It returns errRunLocked if another holder has it.
func acquireRunLock(run string) (*runLock, error) {
	if !nameRx.MatchString(run) {
		return nil, fmt.Errorf("run %q: must match %s", run, nameRx)
	}
	if err := os.MkdirAll(runLockDir(), 0o700); err != nil {
		return nil, fmt.Errorf("run lock dir: %w", err)
	}
	// Lock files are never removed: unlinking one would let a waiter lock an orphaned inode while a new holder
	// locks a fresh file, so two holders would both believe they own the run.
	f, err := os.OpenFile(filepath.Join(runLockDir(), run+".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("run %q lock: %w", run, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("run %q: %w", run, errRunLocked)
		}
		return nil, fmt.Errorf("run %q lock: %w", run, err)
	}
	return &runLock{f: f}, nil
}

// release drops the lock by closing its file. It is safe to call more than once.
func (l *runLock) release() {
	l.once.Do(func() { _ = l.f.Close() })
}

// withRunLock calls fn while holding run's lock, so no other instance can take the run during fn. It returns
// errRunLocked without calling fn when the run is live.
func withRunLock(run string, fn func() error) error {
	l, err := acquireRunLock(run)
	if err != nil {
		return err
	}
	defer l.release()
	return fn()
}

// newRunID returns a random run id for a Registry that no topology names.
func newRunID() string {
	return strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
}

// setRun names the Registry's run, which labels the containers and underlay objects it creates. It fails once the
// run lock is held, since a different run would then leave objects behind the lock.
func (r *Registry) setRun(run string) error {
	r.runMu.Lock()
	defer r.runMu.Unlock()

	if r.lock != nil {
		return fmt.Errorf("run %q: already in use by this model", r.run)
	}
	r.run = run
	return nil
}

// runID returns the Registry's run id, drawing a random one on first use.
func (r *Registry) runID() string {
	r.runMu.Lock()
	defer r.runMu.Unlock()

	if r.run == "" {
		r.run = newRunID()
	}
	return r.run
}

// holdRun takes the lock of the Registry's run, once, before the first labelled object is created.
func (r *Registry) holdRun() error {
	r.runMu.Lock()
	defer r.runMu.Unlock()

	if r.lock != nil {
		return nil
	}
	if r.run == "" {
		r.run = newRunID()
	}
	l, err := acquireRunLock(r.run)
	if err != nil {
		return err
	}
	r.lock = l
	return nil
}

// releaseRun drops the lock taken by holdRun. Containers and underlay objects of the run must be gone first.
func (r *Registry) releaseRun() {
	r.runMu.Lock()
	defer r.runMu.Unlock()

	if r.lock != nil {
		r.lock.release()
		r.lock = nil
	}
}

// containerName returns the name of the container for a VM or host, unique to this Registry's run.
func (r *Registry) containerName(name, uid string) string {
	return constructContainerName(r.runID(), name, uid)
}

// removeExistingContainer removes a container called name left by an earlier create, unless another live run
// owns it. Containers of this run are always removed; containers of a stale run are removed under that run's lock.
func (r *Registry) removeExistingContainer(name string) error {
	out, err := exec.Command("docker", "inspect", "--format", `{{index .Config.Labels "`+labelRun+`"}}`, name).Output()
	if err != nil {
		return nil // no such container
	}

	remove := func() error {
		if _, err := exec.Command("docker", "rm", "-f", name).Output(); err != nil {
			return fmt.Errorf("remove container %s: %w", name, err)
		}
		return nil
	}

	owner := strings.TrimSpace(string(out))
	switch {
	case owner == r.runID():
		return remove()
	case owner == "":
		return fmt.Errorf("container %s has no %s label: not removing it", name, labelRun)
	default:
		err := withRunLock(owner, remove)
		if errors.Is(err, errRunLocked) {
			return fmt.Errorf("container %s belongs to live run %q: %w", name, owner, err)
		}
		return err
	}
}
