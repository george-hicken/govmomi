// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package simulator

import (
	"archive/tar"
	"net/url"
	"strconv"
	"time"

	"github.com/vmware/govmomi/vim25/types"
)

// defaultGuestFileMode is the mode of an uploaded file whose transfer request
// names none.
const defaultGuestFileMode = 0444

// guestFileAttributes are the POSIX attributes a client asks
// InitiateFileTransferToGuest to give the uploaded file. The transfer URL
// carries them from that call to the HTTP PUT that writes the file, as a real
// vCenter's URL carries its transfer id.
type guestFileAttributes struct {
	mode     int64
	uid, gid *int32
}

// posixFileAttributes returns the permissions and owner attr names. Any other
// kind of attributes, or none, names nothing.
func posixFileAttributes(attr types.BaseGuestFileAttributes) guestFileAttributes {
	posix, ok := attr.(*types.GuestPosixFileAttributes)
	if !ok {
		return guestFileAttributes{}
	}
	return guestFileAttributes{mode: posix.Permissions, uid: posix.OwnerId, gid: posix.GroupId}
}

// addTo records the attributes in the transfer URL's query.
func (a guestFileAttributes) addTo(query url.Values) {
	if a.mode != 0 {
		query.Set("mode", strconv.FormatInt(a.mode, 8))
	}
	if a.uid != nil {
		query.Set("uid", strconv.Itoa(int(*a.uid)))
	}
	if a.gid != nil {
		query.Set("gid", strconv.Itoa(int(*a.gid)))
	}
}

// guestFileAttributesFrom is the inverse of addTo. A malformed value names
// nothing, which leaves the default.
func guestFileAttributesFrom(query url.Values) guestFileAttributes {
	var a guestFileAttributes
	if mode, err := strconv.ParseInt(query.Get("mode"), 8, 64); err == nil {
		a.mode = mode
	}
	a.uid = int32Param(query, "uid")
	a.gid = int32Param(query, "gid")
	return a
}

func int32Param(query url.Values, key string) *int32 {
	v, err := strconv.ParseInt(query.Get(key), 10, 32)
	if err != nil {
		return nil
	}
	id := int32(v)
	return &id
}

// tarHeader returns the header that creates the file name of length bytes with
// these attributes in the guest. The owner is root unless named, as for an
// upload authenticated as root.
func (a guestFileAttributes) tarHeader(name string, length int64) *tar.Header {
	h := &tar.Header{
		Name:    name,
		Size:    length,
		Mode:    defaultGuestFileMode,
		ModTime: time.Now(),
	}
	if a.mode != 0 {
		h.Mode = a.mode
	}
	if a.uid != nil {
		h.Uid = int(*a.uid)
	}
	if a.gid != nil {
		h.Gid = int(*a.gid)
	}
	return h
}
