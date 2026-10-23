// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package simulator

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/vmware/govmomi"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/soap"
	"github.com/vmware/govmomi/vim25/types"
)

// TestContainerLifecycleRuntime is opt-in: it runs a local immutable image and
// owns every resource it creates. It never pulls an image or runs an appliance.
func TestContainerLifecycleRuntime(t *testing.T) {
	if os.Getenv("VCSIM_LIFECYCLE_RUNTIME") != "1" {
		t.Skip("set VCSIM_LIFECYCLE_RUNTIME=1 with an immutable local image and artifact directory")
	}
	for _, mode := range []string{"failed-start", "client-cancel", "task-cancel", "normal", "network rm", "volume rm"} {
		t.Run(mode, func(t *testing.T) {
			h := newLifecycleRuntime(t, mode)
			if mode == "failed-start" {
				h.faults(map[string]int{"start": 1})
			}
			phaseStart := time.Now()
			power, err := h.vm.PowerOn(h.ctx)
			require.NoError(t, err)
			if strings.HasSuffix(mode, "cancel") {
				require.Eventually(t, func() bool { _, e := os.Stat(filepath.Join(h.control, "barrier.json")); return e == nil }, 30*time.Second, 20*time.Millisecond)
				if mode == "client-cancel" {
					canceled, cancel := context.WithCancel(h.ctx)
					cancel()
					err = power.Wait(canceled)
					require.Error(t, err)
					h.record("client-wait-cancel", map[string]any{"error": err.Error()})
				} else {
					require.NoError(t, power.Cancel(h.ctx))
					var info mo.Task
					require.NoError(t, power.Properties(h.ctx, power.Reference(), []string{"info"}, &info))
					h.record("task-cancel-request", map[string]any{"state": info.Info.State, "cancelled": info.Info.Cancelled})
				}
				if os.Getenv("VCSIM_LIFECYCLE_ABORT_DURING_CREATE") == "1" {
					t.Fatal("declared fixture failure while real creation barrier is held")
				}
				h.release()
				// Properties uses the original MoRef and waits for the task's VM lock;
				// cancelled task status alone does not prove its Execute has finished.
				var state mo.VirtualMachine
				require.NoError(t, h.vm.Properties(h.ctx, h.vm.Reference(), []string{"runtime.powerState"}, &state))
				require.Equal(t, types.VirtualMachinePowerStatePoweredOn, state.Runtime.PowerState)
				var info mo.Task
				require.NoError(t, power.Properties(h.ctx, power.Reference(), []string{"info"}, &info))
				h.record("server-completed-after-cancel", map[string]any{"powerState": state.Runtime.PowerState, "taskState": info.Info.State, "taskCancelled": info.Info.Cancelled})
			} else if mode == "failed-start" {
				err = power.Wait(h.ctx)
				require.Error(t, err)
			} else {
				err = power.Wait(h.ctx)
				require.NoError(t, err)
			}
			h.record("power-on-outcome", map[string]any{"seconds": time.Since(phaseStart).Seconds(), "error": errorText(err)})
			h.discover()
			h.snapshot("created")
			if mode != "failed-start" {
				phaseStart = time.Now()
				stopped, e := h.vm.PowerOff(h.ctx)
				require.NoError(t, e)
				require.NoError(t, stopped.Wait(h.ctx))
				h.record("power-off", map[string]any{"seconds": time.Since(phaseStart).Seconds()})
			}
			if mode == "failed-start" {
				h.faults(map[string]int{"rm": 1})
			} else if mode == "network rm" || mode == "volume rm" {
				h.faults(map[string]int{mode: 1})
			}
			phaseStart = time.Now()
			first, e := h.vm.Destroy(h.ctx)
			require.NoError(t, e)
			err = first.Wait(h.ctx)
			h.record("destroy-first", map[string]any{"seconds": time.Since(phaseStart).Seconds(), "error": errorText(err)})
			if mode == "failed-start" || mode == "network rm" || mode == "volume rm" {
				require.Error(t, err)
				var state mo.VirtualMachine
				require.NoError(t, h.vm.Properties(h.ctx, h.vm.Reference(), []string{"name"}, &state))
				h.snapshot("failed-destroy-reachable")
				if mode != "failed-start" {
					require.False(t, h.exists("container", h.id), "first removal must already have removed the real container")
					_, absentError := h.command("rm", "-v", "-f", h.id)
					require.NoError(t, absentError, "installed forced removal must tolerate this exact absent container")
					h.record("absent-force-rm", map[string]any{"id": h.id, "exit": 0})
				}
				phaseStart = time.Now()
				retry, e := h.vm.Destroy(h.ctx)
				require.NoError(t, e)
				require.NoError(t, retry.Wait(h.ctx))
				h.record("destroy-retry", map[string]any{"seconds": time.Since(phaseStart).Seconds()})
			} else {
				require.NoError(t, err)
			}
			h.snapshot("natural-outcome")
			for _, r := range h.owned {
				require.False(t, h.exists(r.Kind, r.ID), "natural cleanup left %s %s", r.Kind, r.ID)
			}
			h.checkSentinels()
			h.checkNamespace()
			_, err = h.vm.Destroy(h.ctx)
			require.Error(t, err)
			h.record("control-pass", map[string]any{"mode": mode})
		})
	}
}

type lifecycleResource struct{ Kind, ID, Created string }
type lifecycleRuntime struct {
	t                                                                                          *testing.T
	mode, engine, image, marker, run, name, network, volume, id, control, artifacts, namespace string
	ctx                                                                                        context.Context
	model                                                                                      *Model
	vm                                                                                         *object.VirtualMachine
	sentinel                                                                                   *Registry
	containerPID                                                                               int
	containerStart                                                                             string
	owned, sentinels                                                                           []lifecycleResource
	mu                                                                                         sync.Mutex
}

func newLifecycleRuntime(t *testing.T, mode string) *lifecycleRuntime {
	t.Helper()
	image := os.Getenv("VCSIM_LIFECYCLE_IMAGE")
	require.True(t, strings.HasPrefix(image, "sha256:") && len(image) == 71, "provide immutable VCSIM_LIFECYCLE_IMAGE")
	artifacts := os.Getenv("VCSIM_LIFECYCLE_ARTIFACTS")
	require.True(t, filepath.IsAbs(artifacts), "provide absolute artifact directory")
	engine := os.Getenv("VCSIM_LIFECYCLE_ENGINE")
	if engine == "" {
		engine = "/usr/bin/podman"
	}
	require.True(t, filepath.IsAbs(engine))
	marker := strings.ReplaceAll(uuid.NewString(), "-", "")
	h := &lifecycleRuntime{t: t, mode: mode, engine: engine, image: image, marker: marker, control: t.TempDir(), artifacts: filepath.Join(artifacts, marker)}
	require.NoError(t, os.MkdirAll(h.artifacts, 0700))
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Second)
	h.ctx = ctx
	t.Cleanup(cancel)
	// No owned object exists yet. Fail rather than invoking the production sweep
	// when any unrelated labelled container could be removed by it.
	all := strings.Fields(h.must("ps", "-a", "-q", "--filter", "label="+labelRun))
	require.Empty(t, all, "stale-sweeper preflight must run on a clean simulator host")
	actual := strings.TrimPrefix(strings.TrimSpace(h.must("image", "inspect", image, "--format", "{{.Id}}")), "sha256:")
	require.Equal(t, strings.TrimPrefix(image, "sha256:"), actual)
	graph := strings.TrimSpace(h.must("info", "--format", "{{.Store.GraphRoot}}"))
	expectedGraph := os.Getenv("VCSIM_LIFECYCLE_GRAPH_ROOT")
	require.True(t, filepath.IsAbs(expectedGraph), "provide absolute expected graph root")
	require.Equal(t, expectedGraph, graph)
	require.Equal(t, "true", strings.TrimSpace(h.must("info", "--format", "{{.Host.Security.Rootless}}")), "controls require rootless Podman")
	bin, e := os.ReadFile(engine)
	require.NoError(t, e)
	sum := sha256.Sum256(bin)
	binaryHash := fmt.Sprintf("%x", sum)
	if expected := os.Getenv("VCSIM_LIFECYCLE_ENGINE_SHA256"); expected != "" {
		require.Equal(t, expected, binaryHash)
	}
	boot, _ := os.ReadFile("/proc/sys/kernel/random/boot_id")
	pgid, _ := syscall.Getpgid(os.Getpid())
	h.record("process-provenance", map[string]any{"engineSHA256": binaryHash, "bootID": strings.TrimSpace(string(boot)), "pgid": pgid, "engineVersion": strings.TrimSpace(h.must("--version"))})
	h.record("preflight", map[string]any{"image": image, "engine": engine, "graphRoot": graph, "pid": os.Getpid(), "start": processStart(), "marker": marker})
	model := VPX()
	model.Cluster = 0
	model.Machine = 1
	model.Autostart = false
	h.model = model
	t.Cleanup(model.Remove)
	require.NoError(t, model.Create())
	server := model.Service.NewServer()
	t.Cleanup(server.Close)
	client, err := govmomi.NewClient(ctx, server.URL, true)
	require.NoError(t, err)
	backing := model.Map().Any("VirtualMachine").(*VirtualMachine)
	backing.Runtime.PowerState = types.VirtualMachinePowerStatePoweredOff
	h.vm = object.NewVirtualMachine(client.Client, backing.Self)
	h.run = marker[:12]
	require.NoError(t, model.Map().setRun(h.run))
	require.NoError(t, model.Map().holdRun())
	h.name = model.Map().containerName(backing.Name, backing.uid.String())
	driver, err := os.ReadFile("testdata/container_lifecycle_driver.py")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(h.control, "docker"), driver, 0700))
	t.Setenv("PATH", h.control+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("VCSIM_LIFECYCLE_CONTROL", h.control)
	// Private rootless bridges need no host-global firewall changes.
	t.Setenv("NETAVARK_FW", "none")
	h.faults(map[string]int{})
	t.Cleanup(h.cleanup)
	labels := []string{"--label", "runtime.control=" + marker, "--label", labelRun + "=" + h.run, "--label", labelPID + "=" + strconv.Itoa(os.Getpid()), "--label", "container=" + h.name, "--label", deleteWithContainer}
	netName := "lcnet-" + marker[:16]
	h.owned = append(h.owned, lifecycleResource{Kind: "network", ID: netName})
	h.must(append([]string{"network", "create", "--internal", "--disable-dns"}, append(labels, netName)...)...)
	h.network = h.inspect("network", netName)["id"].(string)
	h.owned[0].ID = h.network
	h.volume = "lcvol-" + marker[:16]
	h.owned = append(h.owned, lifecycleResource{Kind: "volume", ID: h.volume})
	h.must(append([]string{"volume", "create"}, append(labels, h.volume)...)...)
	config := map[string]any{"engine": engine, "run": h.run, "name": h.name, "marker": marker, "barrier": strings.HasSuffix(mode, "cancel"), "commands": filepath.Join(h.artifacts, "commands.jsonl"), "control": h.control, "image": h.image}
	h.write("config.json", config)
	configBytes, _ := json.Marshal(config)
	require.NoError(t, os.WriteFile(filepath.Join(h.artifacts, "owner.json"), configBytes, 0600))
	// Raw create options avoid the volume population helper: this control owns a
	// prelabelled named volume and an anonymous mount, with no extra containers.
	raw := "--pull=never --label runtime.control=" + marker + " -v " + h.volume + ":/named -v /anonymous " + image + " /bin/sh -c " + shellQuote("trap 'exit 0' TERM INT; while :; do sleep 1; done")
	backing.Config.ExtraConfig = []types.BaseOptionValue{&types.OptionValue{Key: ContainerBackingOptionKey, Value: raw}, &types.OptionValue{Key: "RUN.mountdmi", Value: "false"}, &types.OptionValue{Key: "RUN.machineId", Value: "false"}, &types.OptionValue{Key: "RUN.network", Value: netName}}
	backing.svm = createSimulationVM(backing)
	h.makeSentinels()
	h.record("owner-ready", map[string]any{"run": h.run, "name": h.name, "network": h.network, "volume": h.volume})
	return h
}

func (h *lifecycleRuntime) makeSentinels() {
	h.sentinel = NewRegistry()
	require.NoError(h.t, h.sentinel.setRun("s"+h.marker[:11]))
	require.NoError(h.t, h.sentinel.holdRun())
	name := "sentinel-" + h.marker[:16]
	labels := []string{"--label", "runtime.control=" + h.marker, "--label", labelRun + "=" + h.sentinel.runID(), "--label", labelPID + "=" + strconv.Itoa(os.Getpid()), "--label", "container=" + name, "--label", deleteWithContainer}
	network := name + "-net"
	h.sentinels = append(h.sentinels, lifecycleResource{Kind: "network", ID: network})
	h.must(append([]string{"network", "create", "--internal", "--disable-dns"}, append(labels, network)...)...)
	id := h.inspect("network", network)["id"].(string)
	h.sentinels[0].ID = id
	volume := name + "-vol"
	h.sentinels = append(h.sentinels, lifecycleResource{Kind: "volume", ID: volume})
	h.must(append([]string{"volume", "create"}, append(labels, volume)...)...)
	args := append([]string{"create", "--pull=never", "--name", name, "--network", network, "-v", volume + ":/sentinel"}, labels...)
	args = append(args, h.image, "/bin/true")
	h.sentinels = append(h.sentinels, lifecycleResource{Kind: "container", ID: name})
	id = strings.TrimSpace(h.must(args...))
	h.sentinels[2].ID = id
	h.snapshot("sentinels-created")
}

func processStart() string { return lifecycleProcessStart(os.Getpid()) }
func lifecycleProcessStart(pid int) string {
	b, e := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if e != nil {
		return ""
	}
	text := string(b)
	last := strings.LastIndex(text, ") ")
	if last < 0 {
		return "unknown"
	}
	fields := strings.Fields(text[last+2:])
	if len(fields) > 19 {
		return fields[19]
	}
	return "unknown"
}
func (h *lifecycleRuntime) write(name string, value any) {
	b, e := json.Marshal(value)
	require.NoError(h.t, e)
	temp := filepath.Join(h.control, name+".tmp")
	require.NoError(h.t, os.WriteFile(temp, b, 0600))
	require.NoError(h.t, os.Rename(temp, filepath.Join(h.control, name)))
}
func (h *lifecycleRuntime) faults(value map[string]int) { h.write("faults.json", value) }
func (h *lifecycleRuntime) release() {
	if e := os.WriteFile(filepath.Join(h.control, "release"), nil, 0600); e != nil {
		h.t.Error(e)
	}
}
func (h *lifecycleRuntime) record(stage string, value any) {
	h.mu.Lock()
	defer h.mu.Unlock()
	b, _ := json.Marshal(map[string]any{"utc": time.Now().UTC(), "stage": stage, "mode": h.mode, "value": value})
	f, e := os.OpenFile(filepath.Join(h.artifacts, "stages.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if e != nil {
		h.t.Error(e)
		return
	}
	defer f.Close()
	_, _ = f.Write(append(b, '\n'))
}
func (h *lifecycleRuntime) command(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	start := time.Now()
	cmd := exec.CommandContext(ctx, h.engine, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	h.record("engine", map[string]any{"args": args, "seconds": time.Since(start).Seconds(), "exit": exitCode(err), "stderr": stderr.String()})
	if err != nil {
		return string(out) + stderr.String(), err
	}
	return string(out), nil
}
func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var e *exec.ExitError
	if errors.As(err, &e) {
		return e.ExitCode()
	}
	return -1
}
func (h *lifecycleRuntime) must(args ...string) string {
	h.t.Helper()
	out, e := h.command(args...)
	require.NoError(h.t, e, "engine %v: %s", args, out)
	return out
}
func (h *lifecycleRuntime) rawInspect(kind, id string) (map[string]any, error) {
	out, err := h.command(kind, "inspect", id)
	if err != nil {
		return nil, err
	}
	var values []map[string]any
	if err = json.Unmarshal([]byte(out), &values); err != nil {
		return nil, err
	}
	if len(values) != 1 {
		return nil, errors.New("expected one exact engine object")
	}
	return values[0], nil
}
func (h *lifecycleRuntime) inspect(kind, id string) map[string]any {
	v, e := h.rawInspect(kind, id)
	require.NoError(h.t, e)
	return v
}
func (h *lifecycleRuntime) rawExists(kind, id string) (bool, error) {
	_, err := h.command(kind, "exists", id)
	if err == nil {
		return true, nil
	}
	if exitCode(err) == 1 {
		return false, nil
	}
	return false, err
}
func (h *lifecycleRuntime) exists(kind, id string) bool {
	present, e := h.rawExists(kind, id)
	require.NoError(h.t, e, "unclassified existence error for %s %s", kind, id)
	return present
}
func (h *lifecycleRuntime) loadCreated() error {
	var created struct {
		ID      string `json:"id"`
		Volumes []struct{ Name, CreatedAt string }
	}
	b, e := os.ReadFile(filepath.Join(h.control, "created.json"))
	if e != nil {
		return e
	}
	if e = json.Unmarshal(b, &created); e != nil {
		return e
	}
	if created.ID == "" {
		return errors.New("empty created identity")
	}
	h.id = created.ID
	found := false
	for _, r := range h.owned {
		if r.Kind == "container" && r.ID == h.id {
			found = true
		}
	}
	if !found {
		h.owned = append(h.owned, lifecycleResource{Kind: "container", ID: h.id})
		for _, v := range created.Volumes {
			if v.Name != h.volume {
				h.owned = append(h.owned, lifecycleResource{Kind: "volume", ID: v.Name, Created: v.CreatedAt})
			}
		}
	}
	return nil
}
func (h *lifecycleRuntime) discover() {
	require.NoError(h.t, h.loadCreated())
	if h.exists("container", h.id) {
		v := h.inspect("container", h.id)
		image, _ := v["Image"].(string)
		require.Equal(h.t, strings.TrimPrefix(h.image, "sha256:"), strings.TrimPrefix(image, "sha256:"))
		if state, ok := v["State"].(map[string]any); ok {
			if pid, ok := state["Pid"].(float64); ok && pid > 0 {
				h.containerPID = int(pid)
				h.containerStart = lifecycleProcessStart(h.containerPID)
			}
		}
		h.record("actual-container", map[string]any{"id": h.id, "image": image, "pid": h.containerPID, "start": h.containerStart})
		if ns, ok := v["NetworkSettings"].(map[string]any); ok {
			if p, ok := ns["SandboxKey"].(string); ok && p != "" {
				h.namespace = p
			}
		}
	}
}
func (h *lifecycleRuntime) snapshot(stage string) {
	states := []map[string]any{}
	for _, r := range append(append([]lifecycleResource{}, h.owned...), h.sentinels...) {
		present, e := h.rawExists(r.Kind, r.ID)
		state := "absent"
		if present {
			state = "present"
		}
		if e != nil {
			state = "unknown"
		}
		states = append(states, map[string]any{"resource": r, "state": state, "error": errorText(e)})
	}
	value := map[string]any{"marker": h.marker, "run": h.run, "pid": os.Getpid(), "start": processStart(), "container": h.id, "owned": h.owned, "sentinels": h.sentinels, "namespace": h.namespace, "containerPID": h.containerPID, "containerStart": h.containerStart, "states": states}
	h.record(stage, value)
	b, _ := json.MarshalIndent(value, "", "  ")
	if e := os.WriteFile(filepath.Join(h.artifacts, "ledger.json"), b, 0600); e != nil {
		h.t.Error(e)
	}
}
func errorText(e error) string {
	if e != nil {
		return e.Error()
	}
	return ""
}
func (h *lifecycleRuntime) checkSentinels() {
	for _, r := range h.sentinels {
		require.True(h.t, h.exists(r.Kind, r.ID), "foreign-owner sentinel changed")
		h.verify(r)
	}
}
func (h *lifecycleRuntime) ownedIdentity(r lifecycleResource) error {
	v, e := h.rawInspect(r.Kind, r.ID)
	if e != nil {
		return e
	}
	labels, _ := v["Labels"].(map[string]any)
	if r.Kind == "network" {
		labels, _ = v["labels"].(map[string]any)
	}
	if r.Kind == "container" {
		cfg, _ := v["Config"].(map[string]any)
		labels, _ = cfg["Labels"].(map[string]any)
	}
	if r.Kind == "volume" && r.Created != "" {
		if v["CreatedAt"] != r.Created {
			return errors.New("anonymous volume creation identity changed")
		}
		return nil
	}
	if labels["runtime.control"] != h.marker {
		return errors.New("resource owner marker changed")
	}
	return nil
}
func (h *lifecycleRuntime) verify(r lifecycleResource) { require.NoError(h.t, h.ownedIdentity(r)) }
func (h *lifecycleRuntime) checkNamespace() {
	if h.containerPID > 0 && h.containerStart != "" {
		require.NotEqual(h.t, h.containerStart, lifecycleProcessStart(h.containerPID), "same owned container process survived deletion")
		h.record("container-process-absent", map[string]any{"pid": h.containerPID, "start": h.containerStart})
	}
	if h.namespace == "" {
		h.record("namespace-unknown", "engine exposed no sandbox path")
		return
	}
	_, e := os.Stat(h.namespace)
	require.True(h.t, os.IsNotExist(e), "sandbox namespace still exists or could not be classified: %v", e)
	h.record("namespace-absent", h.namespace)
}
func (h *lifecycleRuntime) rescueDiscover() {
	if h.id != "" {
		return
	}
	out, e := h.command("ps", "-a", "-q", "--no-trunc", "--filter", "label=runtime.control="+h.marker, "--filter", "label="+labelRun+"="+h.run)
	if e != nil {
		h.t.Error(e)
		return
	}
	for _, id := range strings.Fields(out) {
		r := lifecycleResource{Kind: "container", ID: id}
		if e = h.ownedIdentity(r); e != nil {
			h.t.Error(e)
			continue
		}
		h.id = id
		h.owned = append(h.owned, r)
		v, e := h.rawInspect("container", id)
		if e != nil {
			h.t.Error(e)
			continue
		}
		mounts, _ := v["Mounts"].([]any)
		for _, raw := range mounts {
			m, _ := raw.(map[string]any)
			name, _ := m["Name"].(string)
			if m["Type"] == "volume" && name != "" && name != h.volume {
				vol, e := h.rawInspect("volume", name)
				if e != nil {
					h.t.Error(e)
					continue
				}
				created, _ := vol["CreatedAt"].(string)
				h.owned = append(h.owned, lifecycleResource{Kind: "volume", ID: name, Created: created})
			}
		}
	}
}

// lifecycleOwnerWaitResolved accepts only completion or a typed missing VM.
// Resource absence alone cannot resolve a server operation still in flight.
func lifecycleOwnerWaitResolved(err error) bool {
	if err == nil {
		return true
	}
	if soap.IsSoapFault(err) {
		switch soap.ToSoapFault(err).VimFault().(type) {
		case types.ManagedObjectNotFound, *types.ManagedObjectNotFound:
			return true
		}
	}
	return false
}

func TestLifecycleCleanupOwnerWait(t *testing.T) {
	fault := func(value any) error {
		f := &soap.Fault{}
		f.Detail.Fault = value
		return soap.WrapSoapFault(f)
	}
	for _, tc := range []struct {
		name     string
		err      error
		resolved bool
	}{
		{"completed", nil, true},
		{"missing-value", fault(types.ManagedObjectNotFound{}), true},
		{"missing-pointer", fault(&types.ManagedObjectNotFound{}), true},
		{"deadline", context.DeadlineExceeded, false},
		{"cancelled", context.Canceled, false},
		{"transport", errors.New("connection lost"), false},
		{"untyped-missing", errors.New("ManagedObjectNotFound"), false},
		{"other-fault", fault(types.InvalidState{}), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.resolved, lifecycleOwnerWaitResolved(tc.err))
		})
	}
}

func (h *lifecycleRuntime) cleanup() {
	h.release()
	if e := os.WriteFile(filepath.Join(h.control, "faults.json"), []byte("{}"), 0600); e != nil {
		h.t.Error(e)
	}
	// Waiting on the VM lock separates server completion from cancelled task status.
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	var vm mo.VirtualMachine
	var ownerWaitErr error
	if h.vm != nil {
		ownerWaitErr = h.vm.Properties(ctx, h.vm.Reference(), []string{"name"}, &vm)
	}
	ownershipUnknown := !lifecycleOwnerWaitResolved(ownerWaitErr)
	h.record("owner-wait", map[string]any{"ownershipUnknown": ownershipUnknown, "error": errorText(ownerWaitErr)})
	if ownershipUnknown {
		h.t.Errorf("server creation ownership unresolved: %v", ownerWaitErr)
	}
	if h.id == "" {
		if e := h.loadCreated(); e != nil && !os.IsNotExist(e) {
			h.t.Error(e)
		}
	}
	h.rescueDiscover()
	h.snapshot("pre-rescue")
	resources := append(append([]lifecycleResource{}, h.owned...), h.sentinels...)
	for _, kind := range []string{"container", "volume", "network"} {
		for _, r := range resources {
			if r.Kind != kind {
				continue
			}
			present, e := h.rawExists(r.Kind, r.ID)
			if e != nil {
				h.t.Errorf("owned rescue existence unknown: %v", e)
				continue
			}
			if present {
				if e = h.ownedIdentity(r); e != nil {
					h.t.Errorf("owned rescue identity: %v", e)
					continue
				}
				out, e := h.command(r.Kind, "rm", "-f", r.ID)
				if e != nil {
					h.t.Errorf("owned rescue %s %s: %v %s", r.Kind, r.ID, e, out)
				}
			}
		}
	}
	residue, unknown := 0, 0
	for _, r := range resources {
		present, e := h.rawExists(r.Kind, r.ID)
		if e != nil {
			unknown++
			h.t.Errorf("owned final state unknown: %s %s %v", r.Kind, r.ID, e)
		}
		if present {
			residue++
			h.t.Errorf("owned residue: %s %s", r.Kind, r.ID)
		}
	}
	if h.sentinel != nil && residue == 0 && unknown == 0 {
		h.sentinel.releaseRun()
	}
	h.record("rescue-complete", map[string]any{"ownedResources": len(resources), "residue": residue, "unknown": unknown, "zeroObservedResourceResidue": residue == 0 && unknown == 0, "ownershipUnknown": ownershipUnknown, "ownerWaitError": errorText(ownerWaitErr), "zeroResidue": residue == 0 && unknown == 0 && !ownershipUnknown, "functionalFailure": h.t.Failed()})
}
