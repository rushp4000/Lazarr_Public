package vfs

import (
	"context"
	"testing"

	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/your-username/lazarr/internal/catalog"
)

// Workstream G: nodes report the epoch unless stable mtime is on.
func TestSetTimesAndNodeMtime(t *testing.T) {
	var a fuse.Attr
	setTimes(&a, 0)
	if a.Mtime != 0 || a.Atime != 0 || a.Ctime != 0 {
		t.Fatal("0 must leave the attrs untouched")
	}
	setTimes(&a, 42)
	if a.Mtime != 42 || a.Atime != 42 || a.Ctime != 42 {
		t.Fatalf("got %+v", a)
	}

	var out fuse.AttrOut
	if (&fileNode{mtime: 7}).Getattr(context.Background(), nil, &out); out.Mtime != 7 {
		t.Fatalf("fileNode mtime %d", out.Mtime)
	}
	out = fuse.AttrOut{}
	if (&dirNode{mtime: 9}).Getattr(context.Background(), nil, &out); out.Mtime != 9 {
		t.Fatalf("dirNode mtime %d", out.Mtime)
	}
	out = fuse.AttrOut{}
	if (&fileNode{}).Getattr(context.Background(), nil, &out); out.Mtime != 0 {
		t.Fatalf("default fileNode mtime %d", out.Mtime)
	}

	rel := &catalog.Release{AddedOn: 1_700_000_000}
	if got := (&rootNode{}).mtimeFor(rel); got != 0 {
		t.Fatalf("stable off: %d", got)
	}
	if got := (&rootNode{stableMtime: true}).mtimeFor(rel); got != 1_700_000_000 {
		t.Fatalf("stable on: %d", got)
	}
	if got := (&rootNode{stableMtime: true}).mtimeFor(&catalog.Release{}); got != 0 {
		t.Fatalf("no AddedOn: %d", got)
	}
}
