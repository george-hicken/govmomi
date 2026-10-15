// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package simulator

import (
	"testing"
	"time"
)

// setTimingForTest sets the promote knobs for one test and restores them, along with any
// seeded jitter source, when the test ends.
func setTimingForTest(t *testing.T, perGiB time.Duration, jitter float64) {
	t.Helper()
	savedPerGiB, savedJitter := PromoteDisksPerGiB, PromoteDisksJitter
	PromoteDisksPerGiB, PromoteDisksJitter = perGiB, jitter
	t.Cleanup(func() {
		PromoteDisksPerGiB, PromoteDisksJitter = savedPerGiB, savedJitter
		timingRandMu.Lock()
		timingRand = nil
		timingRandMu.Unlock()
	})
}

func TestPromoteDelayDefault(t *testing.T) {
	// The default is the upstream rate, 1 ms per MiB: 1.024 s per GiB.
	if PromoteDisksPerGiB != 1024*time.Millisecond {
		t.Fatalf("PromoteDisksPerGiB default = %v, want 1.024s", PromoteDisksPerGiB)
	}
	if PromoteDisksJitter != 0 {
		t.Fatalf("PromoteDisksJitter default = %v, want 0", PromoteDisksJitter)
	}
	if got := promoteDelay(1 << 30); got != 1024*time.Millisecond {
		t.Errorf("promoteDelay(1 GiB) = %v, want 1.024s", got)
	}
	if got := promoteDelay(48 << 30); got != 48*1024*time.Millisecond {
		t.Errorf("promoteDelay(48 GiB) = %v, want 49.152s", got)
	}
}

func TestPromoteDelayDisabled(t *testing.T) {
	setTimingForTest(t, 0, 0.5)
	if got := promoteDelay(48 << 30); got != 0 {
		t.Errorf("promoteDelay with PerGiB 0 = %v, want 0", got)
	}
	setTimingForTest(t, time.Second, 0.5)
	if got := promoteDelay(0); got != 0 {
		t.Errorf("promoteDelay(0 bytes) = %v, want 0", got)
	}
	if got := promoteDelay(-1); got != 0 {
		t.Errorf("promoteDelay(-1 byte) = %v, want 0", got)
	}
}

func TestPromoteDelayNeverNegative(t *testing.T) {
	// A coefficient of variation above 1 makes the normal draw go negative often.
	setTimingForTest(t, time.Second, 5)
	SeedTiming(1)
	for i := 0; i < 10000; i++ {
		if got := promoteDelay(1 << 30); got < 0 {
			t.Fatalf("promoteDelay draw %d = %v, want >= 0", i, got)
		}
	}
}

func TestSeedTimingReproducible(t *testing.T) {
	setTimingForTest(t, time.Second, 0.2)
	draw := func() []time.Duration {
		SeedTiming(42)
		out := make([]time.Duration, 20)
		for i := range out {
			out[i] = promoteDelay(1 << 30)
		}
		return out
	}
	first, second := draw(), draw()
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("draw %d: %v then %v with the same seed", i, first[i], second[i])
		}
	}
}
