// Package fsmodel is an in-memory file system made of inodes, the shape the
// crash states are built in. Files and directories are inodes; a directory
// maps names to inode numbers. Namespace operations name the directory inode
// they change, so an operation can be applied even when an earlier one it
// depended on was lost: a write to a file whose creation never reached the
// disk lands on an inode no directory points to, which is exactly what a
// real disk would hold.
package fsmodel

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ahmed-hashim-pro/wal-crash/internal/trace"
)

// Root is the inode number of the traced directory.
const Root = 1

type Inode struct {
	Dir     bool
	Data    []byte
	Entries map[string]int
}

type FS struct {
	Inodes map[int]*Inode
	Next   int // next free inode number
}

func New() *FS {
	return &FS{Inodes: map[int]*Inode{Root: {Dir: true, Entries: map[string]int{}}}, Next: Root + 1}
}

// FromInitial builds the starting directory of a trace.
func FromInitial(files []trace.File) (*FS, error) {
	fs := New()
	sorted := append([]trace.File(nil), files...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })
	for _, f := range sorted {
		dir, name := path.Split(f.Path)
		parent, ok := fs.Lookup(strings.TrimSuffix(dir, "/"))
		if !ok || !fs.Inodes[parent].Dir {
			return nil, fmt.Errorf("initial file %s: parent directory missing", f.Path)
		}
		ino := fs.Next
		fs.Next++
		if f.Dir {
			fs.Inodes[ino] = &Inode{Dir: true, Entries: map[string]int{}}
		} else {
			fs.Inodes[ino] = &Inode{Data: append([]byte(nil), f.Data...)}
		}
		fs.Inodes[parent].Entries[name] = ino
	}
	return fs, nil
}

func (fs *FS) Clone() *FS {
	c := &FS{Inodes: make(map[int]*Inode, len(fs.Inodes)), Next: fs.Next}
	for n, in := range fs.Inodes {
		cp := &Inode{Dir: in.Dir}
		if in.Dir {
			cp.Entries = make(map[string]int, len(in.Entries))
			for k, v := range in.Entries {
				cp.Entries[k] = v
			}
		} else {
			// Data is copy-on-write: Apply never mutates a slice in place.
			cp.Data = in.Data
		}
		c.Inodes[n] = cp
	}
	return c
}

// Lookup resolves a slash-separated path relative to the root ("" and "."
// are the root itself).
func (fs *FS) Lookup(rel string) (int, bool) {
	ino := Root
	for _, part := range strings.Split(rel, "/") {
		if part == "" || part == "." {
			continue
		}
		in := fs.Inodes[ino]
		if in == nil || !in.Dir {
			return 0, false
		}
		next, ok := in.Entries[part]
		if !ok {
			return 0, false
		}
		ino = next
	}
	return ino, true
}

func (fs *FS) inode(n int, dir bool) *Inode {
	in := fs.Inodes[n]
	if in == nil {
		in = &Inode{Dir: dir}
		if dir {
			in.Entries = map[string]int{}
		}
		fs.Inodes[n] = in
		if n >= fs.Next {
			fs.Next = n + 1
		}
	}
	if dir && in.Entries == nil {
		in.Entries = map[string]int{}
	}
	return in
}

// Torn says how much of a write reached the disk.
type Torn int

const (
	Whole Torn = iota
	// Half: the first half of the bytes were written; the rest of the range
	// kept its old contents, or reads as zeros where the write extended the
	// file.
	Half
	// Short: only the first half of the bytes were written and the file
	// ends there.
	Short
	// Zeros: the file grew to cover the write but none of its bytes were
	// written, so the new range reads as zeros.
	Zeros
)

func (t Torn) String() string {
	return [...]string{"whole", "half", "short", "zeros"}[t]
}

// Split is where a torn write is cut: half way, rounded down to a 512-byte
// sector when the write spans more than two sectors.
func Split(n int) int {
	h := n / 2
	if n >= 1024 {
		h -= h % 512
	}
	return h
}

// Apply applies one operation. Ops that only mark sync points or output do
// nothing. Applying is tolerant by design: a rename or unlink only removes
// the entry if it still points at the op's inode, and a write to an inode
// nobody created yet creates it unlinked.
func (fs *FS) Apply(o *trace.Op, torn Torn) {
	switch o.Kind {
	case trace.Create:
		fs.inode(o.Ino, false)
		fs.inode(o.Dir, true).Entries[o.Name] = o.Ino
	case trace.Mkdir:
		fs.inode(o.Ino, true)
		fs.inode(o.Dir, true).Entries[o.Name] = o.Ino
	case trace.Rename:
		from := fs.inode(o.Dir, true)
		if from.Entries[o.Name] == o.Ino {
			delete(from.Entries, o.Name)
		}
		fs.inode(o.Dir2, true).Entries[o.Name2] = o.Ino
	case trace.Unlink, trace.Rmdir:
		d := fs.inode(o.Dir, true)
		if d.Entries[o.Name] == o.Ino {
			delete(d.Entries, o.Name)
		}
	case trace.Truncate:
		in := fs.inode(o.Ino, false)
		in.Data = resize(in.Data, o.Size)
	case trace.Write:
		in := fs.inode(o.Ino, false)
		in.Data = write(in.Data, o.Off, o.Data, torn)
	}
}

func resize(b []byte, n int64) []byte {
	if int64(len(b)) >= n {
		return b[:n:n]
	}
	out := make([]byte, n)
	copy(out, b)
	return out
}

func write(old []byte, off int64, data []byte, torn Torn) []byte {
	end := off + int64(len(data))
	size := int64(len(old))
	h := Split(len(data))
	switch torn {
	case Short:
		end = off + int64(h)
		data = data[:h]
	case Zeros:
		data = nil
	case Half:
		data = data[:h]
	}
	if end < size {
		end = size
	}
	out := make([]byte, end)
	copy(out, old)
	copy(out[off:], data)
	return out
}

// Hash identifies the visible contents: every path reachable from the root,
// its kind, and its bytes. Unreachable inodes don't count.
func (fs *FS) Hash() [32]byte {
	h := sha256.New()
	fs.Walk(func(p string, in *Inode) {
		fmt.Fprintf(h, "%q %v %d\n", p, in.Dir, len(in.Data))
		h.Write(in.Data)
	})
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// Walk visits every reachable path in sorted order, the root excluded.
func (fs *FS) Walk(fn func(rel string, in *Inode)) {
	seen := map[int]bool{Root: true}
	var walk func(prefix string, ino int)
	walk = func(prefix string, ino int) {
		in := fs.Inodes[ino]
		names := make([]string, 0, len(in.Entries))
		for n := range in.Entries {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			child := in.Entries[n]
			ci := fs.Inodes[child]
			if ci == nil || (ci.Dir && seen[child]) {
				continue
			}
			p := path.Join(prefix, n)
			fn(p, ci)
			if ci.Dir {
				seen[child] = true
				walk(p, child)
			}
		}
	}
	walk("", Root)
}

// Materialize writes the visible tree into dir, which must exist.
func (fs *FS) Materialize(dir string) error {
	var err error
	fs.Walk(func(p string, in *Inode) {
		if err != nil {
			return
		}
		full := filepath.Join(dir, filepath.FromSlash(p))
		if in.Dir {
			err = os.Mkdir(full, 0o755)
		} else {
			err = os.WriteFile(full, in.Data, 0o644)
		}
	})
	return err
}

// Diff compares the model with a real directory and describes the first
// few differences.
func (fs *FS) Diff(dir string) []string {
	var out []string
	want := map[string]*Inode{}
	fs.Walk(func(p string, in *Inode) { want[p] = in })
	got := map[string]bool{}
	filepath.WalkDir(dir, func(full string, d os.DirEntry, err error) error {
		if err != nil || full == dir {
			return nil
		}
		rel, _ := filepath.Rel(dir, full)
		rel = filepath.ToSlash(rel)
		got[rel] = true
		in, ok := want[rel]
		switch {
		case !ok:
			out = append(out, rel+": on disk but not in the recorded model")
		case in.Dir != d.IsDir():
			out = append(out, rel+": file/directory mismatch")
		case !in.Dir:
			b, _ := os.ReadFile(full)
			if !bytes.Equal(b, in.Data) {
				out = append(out, fmt.Sprintf("%s: %d B on disk, %d B in the recorded model (written through mmap?)", rel, len(b), len(in.Data)))
			}
		}
		return nil
	})
	for p := range want {
		if !got[p] {
			out = append(out, p+": in the recorded model but not on disk")
		}
	}
	sort.Strings(out)
	return out
}
