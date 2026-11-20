package vfs

import (
	"context"
	"syscall"
	"testing"

	"github.com/hanwen/go-fuse/v2/fuse"
)

// Unset ownership keeps the historical root-owned read-only tree; set ownership reports
// puid:pgid with the owner write bit so .NET arrs don't see library targets as ReadOnly.
func TestOwnerStamp(t *testing.T) {
	cases := []struct {
		name     string
		o        owner
		dir      bool
		wantMode uint32
	}{
		{"unset file", owner{}, false, syscall.S_IFREG | 0o444},
		{"unset dir", owner{}, true, syscall.S_IFDIR | 0o555},
		{"set file", owner{uid: 1001, gid: 1001}, false, syscall.S_IFREG | 0o664},
		{"set dir", owner{uid: 1001, gid: 1001}, true, syscall.S_IFDIR | 0o775},
		{"gid only", owner{gid: 1001}, false, syscall.S_IFREG | 0o664},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var a fuse.Attr
			tc.o.stamp(&a, tc.dir)
			if a.Mode != tc.wantMode || a.Uid != tc.o.uid || a.Gid != tc.o.gid {
				t.Fatalf("got mode %o uid %d gid %d", a.Mode, a.Uid, a.Gid)
			}
		})
	}

	o := owner{uid: 1001, gid: 1004}
	var out fuse.AttrOut
	(&fileNode{owner: o}).Getattr(context.Background(), nil, &out)
	if out.Mode != syscall.S_IFREG|0o664 || out.Uid != 1001 || out.Gid != 1004 {
		t.Fatalf("fileNode attr %+v", out.Attr)
	}
	out = fuse.AttrOut{}
	(&dirNode{owner: o}).Getattr(context.Background(), nil, &out)
	if out.Mode != syscall.S_IFDIR|0o775 || out.Uid != 1001 {
		t.Fatalf("dirNode attr %+v", out.Attr)
	}
	out = fuse.AttrOut{}
	(&rootNode{owner: o}).Getattr(context.Background(), nil, &out)
	if out.Mode != syscall.S_IFDIR|0o775 || out.Gid != 1004 {
		t.Fatalf("rootNode attr %+v", out.Attr)
	}
}

func TestSetOwnerClampsIDs(t *testing.T) {
	f := &FS{}
	f.SetOwner(-1, 0)
	if f.owner.set() {
		t.Fatalf("non-positive ids must stay unset: %+v", f.owner)
	}
	f.SetOwner(1001, 1001)
	if f.owner != (owner{uid: 1001, gid: 1001}) {
		t.Fatalf("got %+v", f.owner)
	}
}
