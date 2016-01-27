package fuse

import (
	"bazil.org/fuse"
	"bazil.org/fuse/fs"
	"github.com/disorganizer/brig/store"
	"golang.org/x/net/context"

	// Don't panic.
	// This is just to convert a pointer to an inode.
	"unsafe"
)

// Entry is a file inside a directory.
type Entry struct {
	File *store.File
	fs   *FS
}

func (e *Entry) Attr(ctx context.Context, a *fuse.Attr) error {
	// TODO: Store special permissions? Is this allowed?
	a.Mode = 0755
	a.Size = uint64(e.File.Size)
	a.Inode = *(*uint64)(unsafe.Pointer(&e.File))
	return nil
}

func (e *Entry) Open(ctx context.Context, req *fuse.OpenRequest, resp *fuse.OpenResponse) (fs.Handle, error) {
	return &Handle{Entry: e}, nil
}

func (e *Entry) Setattr(ctx context.Context, req *fuse.SetattrRequest, resp *fuse.SetattrResponse) error {
	// TODO: Update {m,c,a}time? Maybe not needed/Unsure when this is called.
	return nil
}
