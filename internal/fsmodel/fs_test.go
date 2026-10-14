package fsmodel

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ahmed-hashim-pro/wal-crash/internal/trace"
)

func TestTornWrites(t *testing.T) {
	old := []byte("AAAA")
	for _, c := range []struct {
		torn Torn
		off  int64
		data string
		want string
	}{
		{Whole, 2, "bbbb", "AAbbbb"},
		{Half, 2, "bbbb", "AAbb\x00\x00"}, // extended part reads as zeros
		{Short, 2, "bbbb", "AAbb"},
		{Zeros, 2, "bbbb", "AA\x00\x00\x00\x00"[0:2] + "AA\x00\x00"},
		{Half, 0, "bbbb", "bbAA"}, // in place: the rest keeps old bytes
	} {
		got := write(old, c.off, []byte(c.data), c.torn)
		if string(got) != c.want {
			t.Errorf("%v at %d: got %q want %q", c.torn, c.off, got, c.want)
		}
	}
	if string(old) != "AAAA" {
		t.Error("write mutated its input")
	}
	if Split(4096) != 2048 || Split(1500) != 512 || Split(9) != 4 {
		t.Errorf("Split: %d %d %d", Split(4096), Split(1500), Split(9))
	}
}

func TestApplyToleratesLostDependencies(t *testing.T) {
	fs := New()
	// The create of inode 5 was lost; its write lands on an unlinked inode.
	fs.Apply(&trace.Op{Kind: trace.Write, Ino: 5, Data: []byte("x")}, Whole)
	if _, ok := fs.Lookup("f"); ok {
		t.Fatal("unlinked inode is visible")
	}
	// A rename whose source entry never existed still links the target.
	fs.Apply(&trace.Op{Kind: trace.Rename, Dir: Root, Name: "tmp", Dir2: Root, Name2: "f", Ino: 5}, Whole)
	if ino, ok := fs.Lookup("f"); !ok || ino != 5 || string(fs.Inodes[5].Data) != "x" {
		t.Fatal("rename did not link the inode")
	}
	// Unlinking a name that now points elsewhere does nothing.
	fs.Apply(&trace.Op{Kind: trace.Unlink, Dir: Root, Name: "f", Ino: 9}, Whole)
	if _, ok := fs.Lookup("f"); !ok {
		t.Fatal("unlink removed the wrong inode's entry")
	}
}

func TestHashSeesOnlyReachableFiles(t *testing.T) {
	a := New()
	b := New()
	b.Apply(&trace.Op{Kind: trace.Write, Ino: 7, Data: []byte("orphan")}, Whole)
	if a.Hash() != b.Hash() {
		t.Error("an unreachable inode changed the hash")
	}
	b.Apply(&trace.Op{Kind: trace.Create, Dir: Root, Name: "f", Ino: 7}, Whole)
	if a.Hash() == b.Hash() {
		t.Error("a visible file did not change the hash")
	}
}

func TestMaterializeAndDiff(t *testing.T) {
	fs, err := FromInitial([]trace.File{{Path: "d", Dir: true}, {Path: "d/f", Data: []byte("hi")}, {Path: "g"}})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := fs.Materialize(dir); err != nil {
		t.Fatal(err)
	}
	if d := fs.Diff(dir); len(d) != 0 {
		t.Fatalf("diff after materialize: %q", d)
	}
	os.WriteFile(filepath.Join(dir, "d/f"), []byte("changed"), 0o644)
	os.WriteFile(filepath.Join(dir, "extra"), nil, 0o644)
	if d := fs.Diff(dir); len(d) != 2 {
		t.Fatalf("diff: %q", d)
	}
}
