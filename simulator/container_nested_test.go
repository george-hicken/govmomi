// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package simulator

import (
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
