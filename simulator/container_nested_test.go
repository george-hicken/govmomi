// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package simulator

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNestedContainerArgsVolumes(t *testing.T) {
	volumeArgs := func(args []string) []string {
		var got []string
		for i := 0; i < len(args)-1; i++ {
			if args[i] == "--volume" {
				got = append(got, args[i+1])
			}
		}
		return got
	}

	tests := []struct {
		name    string
		volumes []string
		want    []string
	}{
		{
			name:    "one volume per path",
			volumes: []string{"/var/lib/containerd", "/var/lib/kubelet", "/var/lib/etcd"},
			want:    []string{"/lib/modules:/lib/modules:ro", "/var/lib/containerd", "/var/lib/kubelet", "/var/lib/etcd"},
		},
		{
			name:    "no volume",
			volumes: []string{},
			want:    []string{"/lib/modules:/lib/modules:ro"},
		},
		{
			name:    "whole var",
			volumes: []string{"/var"},
			want:    []string{"/lib/modules:/lib/modules:ro", "/var"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			args := nestedContainerArgs(tc.volumes)
			assert.Equal(t, tc.want, volumeArgs(args))
		})
	}
}

func TestNestedContainerArgsFlags(t *testing.T) {
	args := nestedContainerArgs(nil)
	for _, flag := range []string{"--cgroupns=private", "seccomp=unconfined", "apparmor=unconfined", "/tmp", "/run", "/dev/fuse"} {
		assert.True(t, slices.Contains(args, flag), "missing %s", flag)
	}
	assert.NotContains(t, args, "--cgroupns=host")
}

func TestParseNestedVolumes(t *testing.T) {
	paths, err := parseNestedVolumes(`["/var/lib/containerd","/var/lib/kubelet"]`)
	require.NoError(t, err)
	assert.Equal(t, []string{"/var/lib/containerd", "/var/lib/kubelet"}, paths)

	paths, err = parseNestedVolumes(`[]`)
	require.NoError(t, err)
	assert.NotNil(t, paths, "an empty array must mount none, not take the /var default")
	assert.Empty(t, paths)

	for _, value := range []string{
		`/var/lib/containerd`,
		`null`,
		`{"a":"b"}`,
		`["var/lib"]`,
		`["/var/lib/"]`,
		`["/var/../etc"]`,
		`["/"]`,
		`["/data:/var/lib"]`,
		`[1]`,
	} {
		_, err := parseNestedVolumes(value)
		assert.Error(t, err, "value %s", value)
	}
}

func TestNestedModulesVolumes(t *testing.T) {
	root := t.TempDir()
	kernel := filepath.Join(root, "tmp", ".6.12.112-5.ph5-esx")
	require.NoError(t, os.MkdirAll(filepath.Join(kernel, "kernel", "fs"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(kernel, "file"), nil, 0o644))

	link := func(t *testing.T, dir, name, target string) {
		t.Helper()
		require.NoError(t, os.Symlink(target, filepath.Join(dir, name)))
	}

	t.Run("link outside the directory", func(t *testing.T) {
		modules := filepath.Join(root, "lib-modules")
		require.NoError(t, os.MkdirAll(filepath.Join(modules, "6.12.113-1"), 0o755))
		link(t, modules, "6.12.112-5.ph5-esx", kernel)

		// the container path is the link's target, where the dangling link points
		assert.Equal(t, []string{"--volume", kernel + ":" + kernel + ":ro"}, nestedModulesVolumes(modules))
	})

	t.Run("relative link out of the directory", func(t *testing.T) {
		parent := t.TempDir()
		modules := filepath.Join(parent, "lib-modules")
		require.NoError(t, os.MkdirAll(modules, 0o755))
		require.NoError(t, os.MkdirAll(filepath.Join(parent, "kernel"), 0o755))
		link(t, modules, "6.12.112-5.ph5-esx", "../kernel")

		assert.Equal(t, []string{"--volume", filepath.Join(parent, "kernel") + ":/lib/kernel:ro"},
			nestedModulesVolumes(modules))
	})

	t.Run("no link", func(t *testing.T) {
		modules := filepath.Join(root, "plain")
		require.NoError(t, os.MkdirAll(filepath.Join(modules, "6.12.113-1"), 0o755))

		assert.Empty(t, nestedModulesVolumes(modules))
	})

	t.Run("link within the directory", func(t *testing.T) {
		modules := filepath.Join(root, "inner")
		require.NoError(t, os.MkdirAll(filepath.Join(modules, "6.12.113-1"), 0o755))
		link(t, modules, "current", "6.12.113-1")

		assert.Empty(t, nestedModulesVolumes(modules))
	})

	t.Run("dangling link and link to a file", func(t *testing.T) {
		modules := filepath.Join(root, "broken")
		require.NoError(t, os.MkdirAll(modules, 0o755))
		link(t, modules, "gone", filepath.Join(root, "missing"))
		link(t, modules, "file", filepath.Join(kernel, "file"))

		assert.Empty(t, nestedModulesVolumes(modules))
	})

	t.Run("link to a path under /lib/modules in the container", func(t *testing.T) {
		modules := filepath.Join(root, "container-path")
		require.NoError(t, os.MkdirAll(modules, 0o755))
		link(t, modules, "6.12.112-5.ph5-esx", filepath.Join(containerModules, "elsewhere"))

		assert.Empty(t, nestedModulesVolumes(modules))
	})
	t.Run("missing directory", func(t *testing.T) {
		assert.Empty(t, nestedModulesVolumes(filepath.Join(root, "absent")))
	})
}
