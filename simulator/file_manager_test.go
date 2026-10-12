// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package simulator

import (
	"bytes"
	"context"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/vmware/govmomi"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/task"
	"github.com/vmware/govmomi/vim25/types"
)

// newCopyTestClient starts an ESX simulator and returns a client for it, and the
// on-disk directory backing its datastore. The returned func tears the simulator down.
func newCopyTestClient(t *testing.T) (*govmomi.Client, string, func()) {
	t.Helper()

	m := ESX()
	if err := m.Create(); err != nil {
		t.Fatal(err)
	}

	s := m.Service.NewServer()
	ctx := context.Background()
	c, err := govmomi.NewClient(ctx, s.URL, true)
	if err != nil {
		s.Close()
		m.Remove()
		t.Fatal(err)
	}

	if err = object.NewFileManager(c.Client).MakeDirectory(ctx, "[LocalDS_0] dst", nil, true); err != nil {
		s.Close()
		m.Remove()
		t.Fatal(err)
	}

	dsDir := m.Map().Any("Datastore").(*Datastore).Path()

	return c, dsDir, func() {
		s.Close()
		m.Remove()
	}
}

// createCopyTestDisk creates a thin disk at name with the given capacity. The
// capacity differs between disks so that their contents differ.
func createCopyTestDisk(ctx context.Context, t *testing.T, c *govmomi.Client, name string, capacityKb int64) {
	t.Helper()

	fm := object.NewFileManager(c.Client)
	if err := fm.MakeDirectory(ctx, path.Dir(name), nil, true); err != nil {
		t.Fatal(err)
	}

	dm := object.NewVirtualDiskManager(c.Client)
	spec := &types.FileBackedVirtualDiskSpec{
		VirtualDiskSpec: types.VirtualDiskSpec{
			AdapterType: string(types.VirtualDiskAdapterTypeLsiLogic),
			DiskType:    string(types.VirtualDiskTypeThin),
		},
		CapacityKb: capacityKb,
	}

	task, err := dm.CreateVirtualDisk(ctx, name, nil, spec)
	if err != nil {
		t.Fatal(err)
	}
	if err = task.Wait(ctx); err != nil {
		t.Fatal(err)
	}
}

// copyTestDisk starts a non-force CopyVirtualDisk from src to dst and waits for it.
func copyTestDisk(ctx context.Context, c *govmomi.Client, src, dst string) error {
	dm := object.NewVirtualDiskManager(c.Client)

	task, err := dm.CopyVirtualDisk(ctx, src, nil, dst, nil, nil, false)
	if err != nil {
		return err
	}

	return task.Wait(ctx)
}

// isFileAlreadyExists reports whether err is a task fault of type FileAlreadyExists.
func isFileAlreadyExists(err error) bool {
	te, ok := err.(task.Error)
	if !ok {
		return false
	}

	_, ok = te.Fault().(*types.FileAlreadyExists)
	return ok
}

func TestCopyVirtualDiskConcurrentIdentical(t *testing.T) {
	setLenientCopy(t, true)
	ctx := context.Background()

	c, _, cleanup := newCopyTestClient(t)
	defer cleanup()

	src := "[LocalDS_0] src/src.vmdk"
	dst := "[LocalDS_0] dst/dst.vmdk"
	createCopyTestDisk(ctx, t, c, src, 1024*1024)

	// Several identical non-force copies to one destination, as vm-operator issues
	// when concurrent reconciles cache the same image disk. Each must succeed, and
	// the destination must end up with the source's content.
	const n = 4
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = copyTestDisk(ctx, c, src, dst)
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("copy %d: %v", i, err)
		}
	}

	// A later identical copy, after the racers finished, must also succeed.
	if err := copyTestDisk(ctx, c, src, dst); err != nil {
		t.Errorf("sequential identical copy: %v", err)
	}
}

func TestCopyVirtualDiskExistingDifferentFile(t *testing.T) {
	setLenientCopy(t, true)
	ctx := context.Background()

	c, _, cleanup := newCopyTestClient(t)
	defer cleanup()

	src := "[LocalDS_0] src/src.vmdk"
	other := "[LocalDS_0] other/other.vmdk"
	dst := "[LocalDS_0] dst/dst.vmdk"
	createCopyTestDisk(ctx, t, c, src, 1024*1024)
	createCopyTestDisk(ctx, t, c, other, 2*1024*1024)

	if err := copyTestDisk(ctx, c, other, dst); err != nil {
		t.Fatal(err)
	}

	// dst holds other's content, so a copy from src must not overwrite it.
	err := copyTestDisk(ctx, c, src, dst)
	if !isFileAlreadyExists(err) {
		t.Errorf("copy over a different existing file: got %v, want FileAlreadyExists", err)
	}

	// A copy from other, whose content dst already holds, still succeeds.
	if err := copyTestDisk(ctx, c, other, dst); err != nil {
		t.Errorf("identical copy over existing dst: %v", err)
	}
}

func TestCopyVirtualDiskConcurrentMixedSources(t *testing.T) {
	setLenientCopy(t, true)
	ctx := context.Background()

	c, _, cleanup := newCopyTestClient(t)
	defer cleanup()

	src := "[LocalDS_0] src/src.vmdk"
	other := "[LocalDS_0] other/other.vmdk"
	dst := "[LocalDS_0] dst/dst.vmdk"
	createCopyTestDisk(ctx, t, c, src, 1024*1024)
	createCopyTestDisk(ctx, t, c, other, 2*1024*1024)

	// Whichever source wins the destination, the losers from the other source
	// must get FileAlreadyExists, and the identical copies must succeed.
	type result struct {
		from string
		err  error
	}
	var (
		wg      sync.WaitGroup
		results = make([]result, 0, 6)
		mu      sync.Mutex
	)
	for _, from := range []string{src, src, src, other, other, other} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := copyTestDisk(ctx, c, from, dst)
			mu.Lock()
			results = append(results, result{from, err})
			mu.Unlock()
		}()
	}
	wg.Wait()

	var winner string
	for _, r := range results {
		if r.err == nil {
			if winner != "" && winner != r.from {
				t.Fatalf("copies from both %s and %s succeeded", winner, r.from)
			}
			winner = r.from
			continue
		}
		if !isFileAlreadyExists(r.err) {
			t.Errorf("copy from %s: %v", r.from, r.err)
		}
	}
	if winner == "" {
		t.Fatal("no copy succeeded")
	}
	for _, r := range results {
		if r.err != nil && r.from == winner {
			t.Errorf("identical copy from winner %s failed: %v", winner, r.err)
		}
	}
}

// uploadCopyTestFile writes content to the datastore path name, which must be in the
// form "[LocalDS_0] dir/file", under dsDir, the datastore's on-disk directory. The
// simulator reads the file as-is, as it does a placeholder disk.
func uploadCopyTestFile(t *testing.T, dsDir, name string, content []byte) {
	t.Helper()

	file := filepath.Join(dsDir, strings.TrimPrefix(name, "[LocalDS_0] "))
	if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, content, 0600); err != nil {
		t.Fatal(err)
	}
}

// copyTestDiskWithSpec starts a CopyVirtualDisk from src to dst with destSpec, without
// force, and waits for it.
func copyTestDiskWithSpec(ctx context.Context, c *govmomi.Client, src, dst string, destSpec types.BaseVirtualDiskSpec) error {
	dm := object.NewVirtualDiskManager(c.Client)

	task, err := dm.CopyVirtualDisk(ctx, src, nil, dst, nil, destSpec, false)
	if err != nil {
		return err
	}

	return task.Wait(ctx)
}

// encryptedCopyTestSpec returns a DestSpec that rewrites the destination descriptor,
// as vm-operator's image cache sends it.
func encryptedCopyTestSpec() *types.FileBackedVirtualDiskSpec {
	return &types.FileBackedVirtualDiskSpec{
		VirtualDiskSpec: types.VirtualDiskSpec{
			AdapterType: string(types.VirtualDiskAdapterTypeLsiLogic),
			DiskType:    string(types.VirtualDiskTypeThin),
		},
		CapacityKb: 1024 * 1024,
		Crypto: &types.CryptoSpecEncrypt{
			CryptoKeyId: types.CryptoKeyId{
				KeyId:      "copy-test-key",
				ProviderId: &types.KeyProviderId{Id: "copy-test-provider"},
			},
		},
	}
}

// setLenientCopy sets lenientCopy for the test and restores it when the test ends. It
// must be called before the simulator starts, as the server reads the setting.
func setLenientCopy(t *testing.T, on bool) {
	t.Helper()

	prev := lenientCopy
	lenientCopy = on
	t.Cleanup(func() { lenientCopy = prev })
}

// copyDestSpecCases are the placeholder sources a DestSpec copy is tested with. The
// flat backing file's content must survive the descriptor rewrite. The large flat file
// has no newlines and is larger than a bufio token, as a real disk's is.
var copyDestSpecCases = []struct {
	name string
	flat []byte
}{
	{name: "placeholder", flat: []byte("placeholder")},
	{name: "large-flat", flat: bytes.Repeat([]byte{'x'}, 70<<10)},
}

// TestCopyVirtualDiskDestSpecRepeat covers identical copies with a DestSpec from
// placeholder sources, which are the disks vcsim's e2e test uploads. With lenient copies
// on, repeats succeed, both sequentially and concurrently.
func TestCopyVirtualDiskDestSpecRepeat(t *testing.T) {
	setLenientCopy(t, true)
	ctx := context.Background()

	for _, tc := range copyDestSpecCases {
		t.Run(tc.name, func(t *testing.T) {
			c, dsDir, cleanup := newCopyTestClient(t)
			defer cleanup()

			src := "[LocalDS_0] src/src.vmdk"
			srcFlat := "[LocalDS_0] src/src-flat.vmdk"
			dst := "[LocalDS_0] dst/dst.vmdk"
			uploadCopyTestFile(t, dsDir, src, []byte("placeholder"))
			uploadCopyTestFile(t, dsDir, srcFlat, tc.flat)

			spec := encryptedCopyTestSpec()

			if err := copyTestDiskWithSpec(ctx, c, src, dst, spec); err != nil {
				t.Fatalf("first copy: %v", err)
			}
			if err := copyTestDiskWithSpec(ctx, c, src, dst, spec); err != nil {
				t.Errorf("sequential repeat copy: %v", err)
			}

			const n = 4
			errs := make([]error, n)
			var wg sync.WaitGroup
			for i := range n {
				wg.Add(1)
				go func() {
					defer wg.Done()
					errs[i] = copyTestDiskWithSpec(ctx, c, src, dst, spec)
				}()
			}
			wg.Wait()

			for i, err := range errs {
				if err != nil {
					t.Errorf("concurrent repeat copy %d: %v", i, err)
				}
			}
		})
	}
}

// TestCopyVirtualDiskDestSpecRepeatOff checks that with lenient copies off, which is the
// default, a DestSpec copy succeeds once and a repeat fails with FileAlreadyExists.
func TestCopyVirtualDiskDestSpecRepeatOff(t *testing.T) {
	setLenientCopy(t, false)
	ctx := context.Background()

	for _, tc := range copyDestSpecCases {
		t.Run(tc.name, func(t *testing.T) {
			c, dsDir, cleanup := newCopyTestClient(t)
			defer cleanup()

			src := "[LocalDS_0] src/src.vmdk"
			srcFlat := "[LocalDS_0] src/src-flat.vmdk"
			dst := "[LocalDS_0] dst/dst.vmdk"
			uploadCopyTestFile(t, dsDir, src, []byte("placeholder"))
			uploadCopyTestFile(t, dsDir, srcFlat, tc.flat)

			spec := encryptedCopyTestSpec()

			if err := copyTestDiskWithSpec(ctx, c, src, dst, spec); err != nil {
				t.Fatalf("first copy: %v", err)
			}
			if err := copyTestDiskWithSpec(ctx, c, src, dst, spec); !isFileAlreadyExists(err) {
				t.Errorf("repeat copy: got %v, want FileAlreadyExists", err)
			}
		})
	}
}

// TestCopyVirtualDiskDestSpecExistingDifferentFile checks that a DestSpec copy still
// fails when the destination holds a different disk.
func TestCopyVirtualDiskDestSpecExistingDifferentFile(t *testing.T) {
	ctx := context.Background()

	c, dsDir, cleanup := newCopyTestClient(t)
	defer cleanup()

	src := "[LocalDS_0] src/src.vmdk"
	other := "[LocalDS_0] other/other.vmdk"
	dst := "[LocalDS_0] dst/dst.vmdk"
	uploadCopyTestFile(t, dsDir, src, []byte("placeholder"))
	uploadCopyTestFile(t, dsDir, "[LocalDS_0] src/src-flat.vmdk", []byte("placeholder"))
	uploadCopyTestFile(t, dsDir, other, []byte("other-placeholder"))
	uploadCopyTestFile(t, dsDir, "[LocalDS_0] other/other-flat.vmdk", []byte("other-placeholder"))

	if err := copyTestDiskWithSpec(ctx, c, other, dst, encryptedCopyTestSpec()); err != nil {
		t.Fatal(err)
	}

	err := copyTestDiskWithSpec(ctx, c, src, dst, encryptedCopyTestSpec())
	if !isFileAlreadyExists(err) {
		t.Errorf("DestSpec copy over a different existing file: got %v, want FileAlreadyExists", err)
	}
}
