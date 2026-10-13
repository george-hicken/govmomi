// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package simulator

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"time"

	"github.com/vmware/govmomi/simulator/esx"
	"github.com/vmware/govmomi/vim25/methods"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/soap"
	"github.com/vmware/govmomi/vim25/types"
	"github.com/vmware/govmomi/vmdk"
)

type FileManager struct {
	mo.FileManager

	// dstMu guards dstLocks. A copy holds the lock for its destination, so a copy
	// racing with another to the same path sees the finished file, not a partial one.
	dstMu    sync.Mutex
	dstLocks map[string]*dstLock
}

// dstLock is the copy lock for one destination path. last is the copy that most
// recently completed onto the path, so that an identical repeat can succeed.
type dstLock struct {
	sync.Mutex
	last *copyRecord
}

// copyRecord identifies a completed copy by its source and DestSpec. size and mtime
// are the destination's stat after the copy, so that a later change to it is seen.
type copyRecord struct {
	src   string
	spec  types.BaseVirtualDiskSpec
	size  int64
	mtime time.Time
}

// lockDst acquires the copy lock for the resolved destination path dst and returns it.
// The caller must call Unlock.
func (f *FileManager) lockDst(dst string) *dstLock {
	f.dstMu.Lock()
	if f.dstLocks == nil {
		f.dstLocks = make(map[string]*dstLock)
	}
	l, ok := f.dstLocks[dst]
	if !ok {
		l = new(dstLock)
		f.dstLocks[dst] = l
	}
	f.dstMu.Unlock()

	l.Lock()
	return l
}

// repeats reports whether dst is still the file that the last completed copy onto it
// made from src with spec. The caller must hold l.
func (l *dstLock) repeats(src, dst string, spec types.BaseVirtualDiskSpec) bool {
	r := l.last
	if r == nil || r.src != src || !reflect.DeepEqual(r.spec, spec) {
		return false
	}

	fi, err := os.Stat(dst)
	return err == nil && fi.Size() == r.size && fi.ModTime().Equal(r.mtime)
}

// record notes that the copy from src with spec onto dst has completed. The caller must
// hold l, and must call it again after any later change to dst.
func (l *dstLock) record(src, dst string, spec types.BaseVirtualDiskSpec) {
	fi, err := os.Stat(dst)
	if err != nil {
		l.last = nil
		return
	}

	l.last = &copyRecord{src: src, spec: spec, size: fi.Size(), mtime: fi.ModTime()}
}

func (f *FileManager) findDatastore(ctx *Context, ref mo.Reference, name string) (*Datastore, types.BaseMethodFault) {
	var refs []types.ManagedObjectReference

	if d, ok := asFolderMO(ref); ok {
		refs = d.ChildEntity
	}
	if p, ok := ref.(*StoragePod); ok {
		refs = p.ChildEntity
	}

	for _, ref := range refs {
		obj := ctx.Map.Get(ref)

		if ds, ok := obj.(*Datastore); ok && ds.Name == name {
			return ds, nil
		}
		if p, ok := obj.(*StoragePod); ok {
			ds, _ := f.findDatastore(ctx, p, name)
			if ds != nil {
				return ds, nil
			}
		}
		if d, ok := asFolderMO(obj); ok {
			ds, _ := f.findDatastore(ctx, d, name)
			if ds != nil {
				return ds, nil
			}
		}
	}

	return nil, &types.InvalidDatastore{Name: name}
}

func (f *FileManager) resolve(ctx *Context, dc *types.ManagedObjectReference, name string, remove ...bool) (string, types.BaseMethodFault) {
	p, fault := parseDatastorePath(name)
	if fault != nil {
		return "", fault
	}

	if dc == nil {
		if ctx.Map.IsESX() {
			dc = &esx.Datacenter.Self
		} else {
			return "", &types.InvalidArgument{InvalidProperty: "dc"}
		}
	}

	folder := ctx.Map.Get(*dc).(*Datacenter).DatastoreFolder

	ds, fault := f.findDatastore(ctx, ctx.Map.Get(folder), p.Datastore)
	if fault != nil {
		return "", fault
	}

	return ds.resolve(ctx, p.Path, remove...), nil
}

func (f *FileManager) fault(name string, err error, fault types.BaseFileFault) types.BaseMethodFault {
	switch {
	case os.IsNotExist(err):
		fault = new(types.FileNotFound)
	case os.IsExist(err):
		fault = new(types.FileAlreadyExists)
	}

	fault.GetFileFault().File = name

	return fault.(types.BaseMethodFault)
}

func (f *FileManager) deleteDatastoreFile(ctx *Context, req *types.DeleteDatastoreFile_Task) types.BaseMethodFault {
	file, fault := f.resolve(ctx, req.Datacenter, req.Name, true)
	if fault != nil {
		return fault
	}

	_, err := os.Stat(file)
	if err != nil {
		if os.IsNotExist(err) {
			return f.fault(file, err, new(types.CannotDeleteFile))
		}
	}

	err = os.RemoveAll(file)
	if err != nil {
		return f.fault(file, err, new(types.CannotDeleteFile))
	}

	return nil
}

func (f *FileManager) DiskDescriptor(ctx *Context, dc *types.ManagedObjectReference, name string) (*vmdk.Descriptor, string, types.BaseMethodFault) {
	path, fault := f.resolve(ctx, dc, name)
	if fault != nil {
		return nil, "", fault
	}

	file, err := os.Open(path)
	if err != nil {
		return nil, "", f.fault(name, err, new(types.FileFault))
	}

	defer file.Close()

	desc, err := vmdk.ParseDescriptor(file)
	if err != nil {
		return nil, "", f.fault(name, err, new(types.FileFault))
	}

	return desc, path, nil
}

// SaveDiskDescriptor writes desc over path through a temporary file renamed into place, so a reader sees
// the old descriptor or the new one, never a partial write.
func (f *FileManager) SaveDiskDescriptor(ctx *Context, desc *vmdk.Descriptor, path string) types.BaseMethodFault {
	tmp := path + ".tmp"
	file, err := os.Create(tmp)
	if err != nil {
		return f.fault(path, err, new(types.FileFault))
	}

	if err = desc.Write(file); err != nil {
		_ = file.Close()
		_ = os.Remove(tmp)
		return f.fault(path, err, new(types.FileFault))
	}

	if err = file.Close(); err != nil {
		_ = os.Remove(tmp)
		return f.fault(path, err, new(types.FileFault))
	}

	if err = os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return f.fault(path, err, new(types.FileFault))
	}

	return nil
}

func (f *FileManager) DeleteDatastoreFileTask(ctx *Context, req *types.DeleteDatastoreFile_Task) soap.HasFault {
	task := CreateTask(f, "deleteDatastoreFile", func(*Task) (types.AnyType, types.BaseMethodFault) {
		return nil, f.deleteDatastoreFile(ctx, req)
	})

	return &methods.DeleteDatastoreFile_TaskBody{
		Res: &types.DeleteDatastoreFile_TaskResponse{
			Returnval: task.Run(ctx),
		},
	}
}

func (f *FileManager) MakeDirectory(ctx *Context, req *types.MakeDirectory) soap.HasFault {
	body := &methods.MakeDirectoryBody{}

	name, fault := f.resolve(ctx, req.Datacenter, req.Name)
	if fault != nil {
		body.Fault_ = Fault("", fault)
		return body
	}

	mkdir := os.Mkdir

	if isTrue(req.CreateParentDirectories) {
		mkdir = os.MkdirAll
	}

	err := mkdir(name, 0700)
	if err != nil {
		fault = f.fault(req.Name, err, new(types.CannotCreateFile))
		body.Fault_ = Fault(err.Error(), fault)
		return body
	}

	body.Res = new(types.MakeDirectoryResponse)
	return body
}

func (f *FileManager) moveDatastoreFile(ctx *Context, req *types.MoveDatastoreFile_Task) types.BaseMethodFault {
	src, fault := f.resolve(ctx, req.SourceDatacenter, req.SourceName)
	if fault != nil {
		return fault
	}

	dst, fault := f.resolve(ctx, req.DestinationDatacenter, req.DestinationName)
	if fault != nil {
		return fault
	}

	if !isTrue(req.Force) {
		_, err := os.Stat(dst)
		if err == nil {
			return f.fault(dst, nil, new(types.FileAlreadyExists))
		}
	}

	err := os.Rename(src, dst)
	if err != nil {
		return f.fault(src, err, new(types.CannotAccessFile))
	}

	return nil
}

func (f *FileManager) MoveDatastoreFileTask(ctx *Context, req *types.MoveDatastoreFile_Task) soap.HasFault {
	task := CreateTask(f, "moveDatastoreFile", func(*Task) (types.AnyType, types.BaseMethodFault) {
		return nil, f.moveDatastoreFile(ctx, req)
	})

	return &methods.MoveDatastoreFile_TaskBody{
		Res: &types.MoveDatastoreFile_TaskResponse{
			Returnval: task.Run(ctx),
		},
	}
}

func (f *FileManager) copyDatastoreFile(ctx *Context, req *types.CopyDatastoreFile_Task) types.BaseMethodFault {
	src, fault := f.resolve(ctx, req.SourceDatacenter, req.SourceName)
	if fault != nil {
		return fault
	}

	dst, fault := f.resolve(ctx, req.DestinationDatacenter, req.DestinationName)
	if fault != nil {
		return fault
	}

	l := f.lockDst(dst)
	defer l.Unlock()

	return f.copyFile(l, src, dst, isTrue(req.Force), nil)
}

// lenientCopy lets a non-force copy onto an existing destination succeed when it repeats
// an identical completed copy, or when the destination already holds the source's bytes.
// vCenter rejects every such copy with FileAlreadyExists, so this is off by default and
// is enabled with VCSIM_LENIENT_COPY=true. It exists for vm-operator's concurrent image
// cache copies.
var lenientCopy = os.Getenv("VCSIM_LENIENT_COPY") == "true"

// copyFile copies the file at src to dst. The caller must hold dst's lock l. Without
// force, an existing dst fails with FileAlreadyExists, as in vCenter. When lenientCopy
// is set, an existing dst is also accepted if the last copy onto it was an identical
// repeat (same src and spec, and dst unchanged since), or if it already holds the same
// bytes as src.
func (f *FileManager) copyFile(l *dstLock, src, dst string, force bool, spec types.BaseVirtualDiskSpec) types.BaseMethodFault {
	if !force {
		if _, err := os.Stat(dst); err == nil {
			if !lenientCopy {
				return f.fault(dst, nil, new(types.FileAlreadyExists))
			}
			if l.repeats(src, dst, spec) {
				return nil
			}
			if same, _ := sameFileContent(src, dst); same {
				l.record(src, dst, spec)
				return nil
			}
			return f.fault(dst, nil, new(types.FileAlreadyExists))
		}
	}

	r, err := os.Open(filepath.Clean(src))
	if err != nil {
		return f.fault(dst, err, new(types.CannotAccessFile))
	}
	defer r.Close()

	w, err := os.Create(dst)
	if err != nil {
		return f.fault(dst, err, new(types.CannotCreateFile))
	}
	defer w.Close()

	if _, err = io.Copy(w, r); err != nil {
		return f.fault(dst, err, new(types.CannotCreateFile))
	}

	l.record(src, dst, spec)
	return nil
}

// sameFileContent reports whether the files at a and b hold the same bytes. Any read
// error is returned, and callers treat it as a difference.
func sameFileContent(a, b string) (bool, error) {
	x, err := os.ReadFile(filepath.Clean(a))
	if err != nil {
		return false, err
	}
	y, err := os.ReadFile(filepath.Clean(b))
	if err != nil {
		return false, err
	}
	return bytes.Equal(x, y), nil
}

func (f *FileManager) CopyDatastoreFileTask(ctx *Context, req *types.CopyDatastoreFile_Task) soap.HasFault {
	task := CreateTask(f, "copyDatastoreFile", func(*Task) (types.AnyType, types.BaseMethodFault) {
		return nil, f.copyDatastoreFile(ctx, req)
	})

	return &methods.CopyDatastoreFile_TaskBody{
		Res: &types.CopyDatastoreFile_TaskResponse{
			Returnval: task.Run(ctx),
		},
	}
}
