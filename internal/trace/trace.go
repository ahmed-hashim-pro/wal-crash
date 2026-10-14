// Package trace is the recorded history of one workload run: the directory
// it started from and every operation it made on that directory, in the
// order the kernel completed them.
package trace

import (
	"encoding/json"
	"fmt"
	"os"
)

// Kind of operation.
type Kind string

const (
	// Namespace operations: they change a directory's entries.
	Create Kind = "create" // Dir/Name -> new file Ino
	Mkdir  Kind = "mkdir"  // Dir/Name -> new directory Ino
	Rename Kind = "rename" // Dir/Name -> Dir2/Name2, moving Ino
	Unlink Kind = "unlink" // remove Dir/Name (file Ino)
	Rmdir  Kind = "rmdir"  // remove Dir/Name (directory Ino)

	// Data operations: they change a file's contents or size.
	Write    Kind = "write"    // Data at Off in Ino
	Truncate Kind = "truncate" // Ino to Size bytes

	// Sync points.
	Fsync    Kind = "fsync"    // file Ino: its data and size are durable
	FsyncDir Kind = "fsyncdir" // directory Ino: its entries are durable
	Sync     Kind = "sync"     // sync(2)/syncfs(2): everything is durable

	// Out is one line the workload printed on standard output. Lines that
	// start with the acknowledgement prefix are acknowledgements: promises
	// the checker holds the program to.
	Out Kind = "out"
)

// Op is one recorded operation.
type Op struct {
	Kind  Kind   `json:"kind"`
	Dir   int    `json:"dir,omitempty"`
	Name  string `json:"name,omitempty"`
	Dir2  int    `json:"dir2,omitempty"`
	Name2 string `json:"name2,omitempty"`
	Ino   int    `json:"ino,omitempty"`
	Off   int64  `json:"off,omitempty"`
	Size  int64  `json:"size,omitempty"`
	Data  []byte `json:"data,omitempty"`
	Line  string `json:"line,omitempty"`
	Ack   bool   `json:"ack,omitempty"`

	// For people: the paths involved (relative to the traced directory)
	// and the strace line this came from.
	Path string `json:"path,omitempty"`
	To   string `json:"to,omitempty"`
	Src  int    `json:"src,omitempty"`
}

// Mutates reports whether the op changes the file system.
func (o *Op) Mutates() bool {
	switch o.Kind {
	case Create, Mkdir, Rename, Unlink, Rmdir, Write, Truncate:
		return true
	}
	return false
}

// Namespace reports whether the op changes directory entries.
func (o *Op) Namespace() bool {
	switch o.Kind {
	case Create, Mkdir, Rename, Unlink, Rmdir:
		return true
	}
	return false
}

// String is a one-line description, e.g. "write data.log 24 B @ 96".
func (o *Op) String() string {
	switch o.Kind {
	case Create:
		return "create   " + o.Path
	case Mkdir:
		return "mkdir    " + o.Path
	case Rename:
		return "rename   " + o.Path + " -> " + o.To
	case Unlink:
		return "unlink   " + o.Path
	case Rmdir:
		return "rmdir    " + o.Path
	case Write:
		return fmt.Sprintf("write    %s  %d B @ %d", o.Path, len(o.Data), o.Off)
	case Truncate:
		return fmt.Sprintf("truncate %s  to %d B", o.Path, o.Size)
	case Fsync:
		return "fsync    " + o.Path
	case FsyncDir:
		return "fsync    " + o.Path + "/  (directory)"
	case Sync:
		return "sync"
	case Out:
		return "stdout   " + o.Line
	}
	return string(o.Kind)
}

// File is one entry of the starting directory.
type File struct {
	Path string `json:"path"`
	Dir  bool   `json:"dir,omitempty"`
	Data []byte `json:"data,omitempty"`
}

// Trace is a recorded run.
type Trace struct {
	Version   int      `json:"version"`
	Command   []string `json:"command"`
	Root      string   `json:"root"`
	AckPrefix string   `json:"ack_prefix"`
	Initial   []File   `json:"initial"`
	Ops       []Op     `json:"ops"`
	Warnings  []string `json:"warnings,omitempty"`
}

const Version = 1

func Load(path string) (*Trace, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var t Trace
	if err := json.Unmarshal(b, &t); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if t.Version != Version {
		return nil, fmt.Errorf("%s: trace version %d, this wal-crash reads version %d", path, t.Version, Version)
	}
	return &t, nil
}

func (t *Trace) Save(path string) error {
	b, err := json.MarshalIndent(t, "", " ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

// Acks returns the indexes of acknowledgement ops.
func (t *Trace) Acks() []int {
	var out []int
	for i := range t.Ops {
		if t.Ops[i].Kind == Out && t.Ops[i].Ack {
			out = append(out, i)
		}
	}
	return out
}
