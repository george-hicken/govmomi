// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package simulator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestVmciToolboxBinary pins that the toolbox path is an explicit input:
// VmciToolboxBinary returns a given executable and refuses, naming ToolboxEnv,
// anything else. It never builds one.
func TestVmciToolboxBinary(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "toolbox")
	plain := filepath.Join(dir, "plain")
	for path, mode := range map[string]os.FileMode{exe: 0o755, plain: 0o644} {
		if err := os.WriteFile(path, nil, mode); err != nil {
			t.Fatal(err)
		}
	}

	for _, tc := range []struct{ name, env, want string }{
		{"unset", "", "is not set"},
		{"relative", "toolbox", "not an absolute path"},
		{"missing", filepath.Join(dir, "none"), "no such file"},
		{"directory", dir, "not an executable file"},
		{"not executable", plain, "not an executable file"},
		{"given", exe, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(ToolboxEnv, tc.env)
			path, err := VmciToolboxBinary()
			if tc.want == "" {
				if err != nil || path != exe {
					t.Fatalf("got %q, %v; want %q", path, err, exe)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), ToolboxEnv) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %q, %v; want a refusal naming %s: %s", path, err, ToolboxEnv, tc.want)
			}
			if VmciToolboxBinaryPath() != "" {
				t.Fatal("VmciToolboxBinaryPath gave a path VmciToolboxBinary refused")
			}
		})
	}
}
