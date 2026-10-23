// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package simulator

import (
	"math/rand"
	"os"
	"sync"
	"time"
)

// Timing knobs. Each variable's default is vcsim's behaviour before the knob existed, so an unconfigured
// simulator is unchanged. They are package variables, like TaskDelay, and are read when the simulator
// does the work they govern: set them before creating a model or serving requests. A harness applies
// its timing profile here; the environment variables VCSIM_EVENT_CONSOLIDATION_WINDOW and
// VCSIM_INSPECT_INTERVAL set the defaults of the two container knobs.
var (
	// PromoteDisksPerGiB is the time PromoteDisks_Task sleeps per GiB of promoted disk capacity.
	// The default, 1 ms per MiB, is roughly 1 s per GiB. 0 disables the sleep.
	PromoteDisksPerGiB = 1024 * time.Millisecond

	// PromoteDisksJitter is the coefficient of variation (stddev/mean) of the promote sleep, as
	// DelayConfig.DelayJitter. 0 means every promote sleeps PromoteDisksPerGiB per GiB.
	PromoteDisksJitter float64

	// ContainerEventConsolidationWindow is how long a container's change notification waits so that
	// a burst of updates consolidates into one inspect.
	ContainerEventConsolidationWindow = envDuration("VCSIM_EVENT_CONSOLIDATION_WINDOW", 250*time.Millisecond)

	// ContainerInspectInterval is how often a watched container is inspected without a change event.
	ContainerInspectInterval = envDuration("VCSIM_INSPECT_INTERVAL", 10*time.Second)

	// ContainerIPAttempts and ContainerIPInterval bound the wait for a started container to report
	// an IP address, which container runtimes assign shortly after start.
	ContainerIPAttempts = 5
	ContainerIPInterval = 500 * time.Millisecond

	// PropertyCollectorBatchWindow is how long WaitForUpdates waits for updates to accumulate before
	// it replies.
	PropertyCollectorBatchWindow = 20 * time.Millisecond

	// TaskWaitPoll is how often Task.Wait checks whether the task has finished.
	TaskWaitPoll = 10 * time.Millisecond

	// PodmanHelperStartPoll is how often a router's netns helper is checked for its socket.
	PodmanHelperStartPoll = 20 * time.Millisecond

	// PodmanHelperStartTimeout bounds how long a router's netns helper may take to start its socket.
	PodmanHelperStartTimeout = 10 * time.Second

	// PodmanHelperStopTimeout bounds how long a router's netns helper may take to exit after
	// shutdown before it is killed.
	PodmanHelperStopTimeout = 5 * time.Second
)

// timingRand, when set by SeedTiming, is the source of jitter draws. It is nil by default, and
// jitter then uses the global math/rand source.
var (
	timingRandMu sync.Mutex
	timingRand   *rand.Rand
)

// SeedTiming makes the jitter draws of promoteDelay and DelayConfig reproducible: the same seed
// yields the same sequence of draws. Draws from concurrent callers share one sequence under a lock,
// so which caller gets which draw still depends on scheduling. Without a call, jitter uses the
// global source.
func SeedTiming(seed int64) {
	timingRandMu.Lock()
	defer timingRandMu.Unlock()
	timingRand = rand.New(rand.NewSource(seed))
}

// normFloat64 returns a standard normal draw from the seeded source if SeedTiming set one.
func normFloat64() float64 {
	timingRandMu.Lock()
	defer timingRandMu.Unlock()
	if timingRand == nil {
		return rand.NormFloat64()
	}
	return timingRand.NormFloat64()
}

// promoteDelay returns the sleep PromoteDisks_Task takes for capacity bytes of promoted disk.
func promoteDelay(capacity int64) time.Duration {
	if PromoteDisksPerGiB <= 0 || capacity <= 0 {
		return 0
	}
	d := time.Duration(float64(PromoteDisksPerGiB) * float64(capacity) / (1 << 30))
	if PromoteDisksJitter > 0 {
		d += time.Duration(normFloat64() * PromoteDisksJitter * float64(d))
	}
	return max(d, 0)
}

// envDuration returns the duration in the environment variable name, or def when it is unset or unparsable.
func envDuration(name string, def time.Duration) time.Duration {
	if d, err := time.ParseDuration(os.Getenv(name)); err == nil {
		return d
	}
	return def
}
