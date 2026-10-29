// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package simulator

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/vmware/govmomi/test"
	"github.com/vmware/govmomi/vim25/types"
)

// runChildDirEnv names the directory TestContainerRunSecondProcess and its child exchange files through.
const runChildDirEnv = "VCSIM_RUN_CHILD_DIR"

// sharedVMBacking is the container backing of a VM that several simulator instances create under the same name
// and paths.
func sharedVMBacking() types.BaseOptionValue {
	return &types.OptionValue{Key: ContainerBackingOptionKey, Value: `["alpine","sh","-c","` + sleepForever + `"]`}
}

// containerRunning reports whether the container called name is running. A missing container is not running.
func containerRunning(name string) bool {
	out, err := exec.Command("docker", "inspect", "--format", "{{.State.Running}}", name).Output()
	return err == nil && strings.TrimSpace(string(out)) == "true"
}

// containerRun returns the run label of the container called name, or "" if it has none.
func containerRun(t *testing.T, name string) string {
	t.Helper()

	out, err := exec.Command("docker", "inspect", "--format", `{{index .Config.Labels "`+labelRun+`"}}`, name).Output()
	require.NoError(t, err)
	return strings.TrimSpace(string(out))
}

// waitForFile returns the contents of path once it is non-empty, failing the test after timeout.
func waitForFile(t *testing.T, path string, timeout time.Duration) string {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for {
		if b, err := os.ReadFile(path); err == nil && len(b) > 0 {
			return string(b)
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", path)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestContainerRunTwoModels runs two Models in one process, each with a VM of the same name and paths. Each VM's
// container is labelled with its own run, and neither Model's teardown removes the other's container.
func TestContainerRunTwoModels(t *testing.T) {
	test.RequireContainerOptIn(t)
	if !test.HasDocker() {
		t.Skip("requires docker or podman (aliased as docker) on linux")
	}

	envA := newVCSIMEnv(t)
	_, simA := powerOnContainerVM(t, envA, "shared-vm", sharedVMBacking())
	nameA := simA.svm.c.name

	envB := newVCSIMEnv(t)
	vmB, simB := powerOnContainerVM(t, envB, "shared-vm", sharedVMBacking())
	nameB := simB.svm.c.name

	require.NotEqual(t, nameA, nameB)
	require.Equal(t, envA.simCtx.Map.runID(), containerRun(t, nameA))
	require.Equal(t, envB.simCtx.Map.runID(), containerRun(t, nameB))
	require.True(t, containerRunning(nameA))
	require.True(t, containerRunning(nameB))

	// Destroying B's VM removes B's container only.
	powerOff(t, envB, vmB)
	task, err := vmB.Destroy(envB.goCtx)
	require.NoError(t, err)
	require.NoError(t, task.Wait(envB.goCtx))

	require.False(t, containerRunning(nameB))
	require.True(t, containerRunning(nameA))
}

// TestContainerRunRecreateRefusesLiveRun checks that RecreateIfExists does not remove a container that a live run
// owns, even under the same name, and that the container survives the refusal.
func TestContainerRunRecreateRefusesLiveRun(t *testing.T) {
	test.RequireContainerOptIn(t)
	if !test.HasDocker() {
		t.Skip("requires docker or podman (aliased as docker) on linux")
	}

	owner := NewRegistry()
	require.NoError(t, owner.holdRun())
	t.Cleanup(owner.releaseRun)

	name := owner.containerName("refused-vm", "uid")
	out, err := exec.Command("docker", "run", "-d", "--pull=never", "--name", name,
		"--label", labelRun+"="+owner.runID(), "alpine:3.20.3", "sleep", "infinity").CombinedOutput()
	require.NoError(t, err, string(out))
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })

	other := NewRegistry()
	err = other.removeExistingContainer(name)
	require.ErrorIs(t, err, errRunLocked)
	require.True(t, containerRunning(name))
}

// TestContainerRunSecondProcess starts a second process that creates a VM with the same name and paths. Each
// process's container survives the other's creation and teardown.
func TestContainerRunSecondProcess(t *testing.T) {
	test.RequireContainerOptIn(t)
	if !test.HasDocker() {
		t.Skip("requires docker or podman (aliased as docker) on linux")
	}

	dir := t.TempDir()
	env := newVCSIMEnv(t)
	_, sim := powerOnContainerVM(t, env, "shared-vm", sharedVMBacking())
	name := sim.svm.c.name

	cmd := exec.Command(os.Args[0], "-test.run=^TestContainerRunChild$")
	cmd.Env = append(os.Environ(), runChildDirEnv+"="+dir)
	cmd.Stderr = os.Stderr
	require.NoError(t, cmd.Start())
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	childName := waitForFile(t, filepath.Join(dir, "ready"), 2*time.Minute)
	require.NotEqual(t, name, childName)
	require.True(t, containerRunning(name))
	require.True(t, containerRunning(childName))

	require.NoError(t, os.WriteFile(filepath.Join(dir, "done"), []byte("done"), 0o600))
	require.NoError(t, cmd.Wait())

	require.False(t, containerRunning(childName))
	require.True(t, containerRunning(name))
}

// TestContainerRunChild is the second process of TestContainerRunSecondProcess. It runs only when that test starts it.
func TestContainerRunChild(t *testing.T) {
	dir := os.Getenv(runChildDirEnv)
	if dir == "" {
		t.Skip("started by TestContainerRunSecondProcess")
	}

	env := newVCSIMEnv(t)
	_, sim := powerOnContainerVM(t, env, "shared-vm", sharedVMBacking())
	require.NoError(t, os.WriteFile(filepath.Join(dir, "ready"), []byte(sim.svm.c.name), 0o600))

	waitForFile(t, filepath.Join(dir, "done"), 2*time.Minute)
}

// TestContainerRunSweepStale checks that a container of a run whose lock is free is swept, and that a container
// of a run whose lock is held is kept.
func TestContainerRunSweepStale(t *testing.T) {
	test.RequireContainerOptIn(t)
	if !test.HasDocker() {
		t.Skip("requires docker or podman (aliased as docker) on linux")
	}

	start := func(run, name string) {
		t.Helper()
		out, err := exec.Command("docker", "run", "-d", "--pull=never", "--name", name,
			"--label", labelRun+"="+run, "--label", labelPID+"=1", "alpine:3.20.3", "sleep", "infinity").CombinedOutput()
		require.NoError(t, err, string(out))
		t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })
	}
	exists := func(name string) bool {
		return exec.Command("docker", "inspect", name).Run() == nil
	}

	stale, live := newRunID(), newRunID()
	start(stale, "vcsim-"+stale+"-sweep-vm")
	start(live, "vcsim-"+live+"-sweep-vm")

	held, err := acquireRunLock(live)
	require.NoError(t, err)
	t.Cleanup(held.release)

	require.NoError(t, sweepContainers())
	require.False(t, exists("vcsim-"+stale+"-sweep-vm"))
	require.True(t, exists("vcsim-"+live+"-sweep-vm"))
}
