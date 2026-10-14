// Package crash enumerates the states a directory may be left in if power
// fails part way through a recorded run, under a stated persistence model.
package crash

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/ahmed-hashim-pro/wal-crash/internal/fsmodel"
	"github.com/ahmed-hashim-pro/wal-crash/internal/trace"
)

// Model is a persistence model: what may survive a crash.
type Model string

const (
	// Process: the program is killed (kill -9, OOM, panic) but the
	// operating system keeps running. Every operation that returned is
	// kept, in order, because it is in the page cache. A crash can only
	// cut the sequence between two operations.
	Process Model = "process"
	// Ordered: power fails on a disk that writes everything in the order
	// it was issued. A crash cuts the sequence anywhere, including in the
	// middle of a write.
	Ordered Model = "ordered"
	// POSIX: power fails and only what fsync promised survives. An
	// operation the program issued but nothing has synced yet may be
	// lost, kept, or torn, independently of the others.
	POSIX Model = "posix"
)

var Models = []Model{Process, Ordered, POSIX}

func ParseModel(s string) (Model, error) {
	for _, m := range Models {
		if string(m) == s {
			return m, nil
		}
	}
	return "", fmt.Errorf("unknown model %q (want process, ordered or posix)", s)
}

// Kind of crash state at a cut point.
type Kind string

const (
	Prefix Kind = ""       // every issued operation persisted
	Synced Kind = "synced" // only what was synced persisted
	Drop   Kind = "drop"   // every issued operation but one persisted
	Only   Kind = "only"   // of the unsynced operations, only one persisted
	Half   Kind = "half"   // one write torn: first half new, rest old or zeros
	Short  Kind = "short"  // one write torn: first half only, file ends there
	Zeros  Kind = "zeros"  // one write torn: file grew, new bytes are zeros
)

// Variant names one crash state.
type Variant struct {
	Cut  int  // operations [0, Cut) were issued before the power failed
	Kind Kind //
	Op   int  // the operation varied (0-based), for every kind but Prefix and Synced
}

// ID is the state's name on the command line, with operations numbered
// from 1 as wal-crash prints them: "c41" (crash after op 41), "c41.drop39",
// "c41.only39", "c41.synced", "c41.half39".
func (v Variant) ID() string {
	s := "c" + strconv.Itoa(v.Cut)
	switch v.Kind {
	case Prefix:
	case Synced:
		s += ".synced"
	default:
		s += "." + string(v.Kind) + strconv.Itoa(v.Op+1)
	}
	return s
}

func ParseID(s string) (Variant, error) {
	bad := fmt.Errorf("bad state id %q (want e.g. c41, c41.synced, c41.drop39)", s)
	if !strings.HasPrefix(s, "c") {
		return Variant{}, bad
	}
	head, tail, _ := strings.Cut(s[1:], ".")
	cut, err := strconv.Atoi(head)
	if err != nil || cut < 0 {
		return Variant{}, bad
	}
	v := Variant{Cut: cut}
	switch {
	case tail == "":
	case tail == "synced":
		v.Kind = Synced
	default:
		i := strings.IndexAny(tail, "0123456789")
		if i <= 0 {
			return Variant{}, bad
		}
		v.Kind = Kind(tail[:i])
		switch v.Kind {
		case Drop, Only, Half, Short, Zeros:
		default:
			return Variant{}, bad
		}
		n, err := strconv.Atoi(tail[i:])
		if err != nil || n < 1 || n > cut {
			return Variant{}, bad
		}
		v.Op = n - 1
	}
	return v, nil
}

// Explorer builds crash states for one trace.
type Explorer struct {
	Trace    *trace.Trace
	initial  *fsmodel.FS
	forcedAt []int  // per op: index of the first sync that makes it durable, or never
	extends  []bool // per write: it grew the file (in the full run)
}

func New(tr *trace.Trace) (*Explorer, error) {
	fs, err := fsmodel.FromInitial(tr.Initial)
	if err != nil {
		return nil, err
	}
	e := &Explorer{Trace: tr, initial: fs}
	e.forcedAt = make([]int, len(tr.Ops))
	e.extends = make([]bool, len(tr.Ops))
	full := fs.Clone()
	for i := range tr.Ops {
		o := &tr.Ops[i]
		e.forcedAt[i] = never
		if o.Mutates() {
			e.forcedAt[i] = forcedAt(tr.Ops, i)
		}
		if o.Kind == trace.Write {
			in := full.Inodes[o.Ino]
			e.extends[i] = in == nil || o.Off+int64(len(o.Data)) > int64(len(in.Data))
		}
		full.Apply(o, fsmodel.Whole)
	}
	return e, nil
}

// forcedAt finds the first sync after op i that covers it: fsync of the
// file for data, fsync of the directory for an entry (both directories for
// a rename between two), or sync(2) for anything.
func forcedAt(ops []trace.Op, i int) int {
	o := &ops[i]
	needA, needB := true, o.Kind == trace.Rename && o.Dir2 != o.Dir
	gotA, gotB := -1, -1
	for j := i + 1; j < len(ops); j++ {
		s := &ops[j]
		switch s.Kind {
		case trace.Sync:
			return j
		case trace.Fsync:
			if (o.Kind == trace.Write || o.Kind == trace.Truncate) && s.Ino == o.Ino {
				return j
			}
		case trace.FsyncDir:
			if !o.Namespace() {
				continue
			}
			if needA && gotA < 0 && s.Ino == o.Dir {
				gotA = j
			}
			if needB && gotB < 0 && s.Ino == o.Dir2 {
				gotB = j
			}
			if gotA >= 0 && (!needB || gotB >= 0) {
				return j
			}
		}
	}
	return never
}

// never marks an operation no sync ever covers.
const never = math.MaxInt

// Forced reports whether op i is durable for a crash at cut.
func (e *Explorer) Forced(i, cut int) bool { return e.forcedAt[i] < cut }

// Pending lists the operations issued before cut that nothing synced.
func (e *Explorer) Pending(cut int) []int {
	var p []int
	for i := 0; i < cut; i++ {
		if e.Trace.Ops[i].Mutates() && !e.Forced(i, cut) {
			p = append(p, i)
		}
	}
	return p
}

// Variants lists every crash state the model allows, as wal-crash explores
// them (see the README for why this is a bounded search, not every subset).
func (e *Explorer) Variants(m Model) []Variant {
	var out []Variant
	ops := e.Trace.Ops
	for cut := 0; cut <= len(ops); cut++ {
		out = append(out, Variant{Cut: cut})
		switch m {
		case Ordered:
			if cut > 0 && ops[cut-1].Kind == trace.Write {
				out = append(out, e.torn(cut, cut-1)...)
			}
		case POSIX:
			p := e.Pending(cut)
			if len(p) > 0 {
				out = append(out, Variant{Cut: cut, Kind: Synced})
			}
			if len(p) > 1 {
				for _, i := range p {
					out = append(out, Variant{Cut: cut, Kind: Drop, Op: i}, Variant{Cut: cut, Kind: Only, Op: i})
				}
			}
			for _, i := range p {
				if ops[i].Kind == trace.Write {
					out = append(out, e.torn(cut, i)...)
				}
			}
		}
	}
	return out
}

func (e *Explorer) torn(cut, i int) []Variant {
	if len(e.Trace.Ops[i].Data) < 2 {
		return nil
	}
	out := []Variant{{Cut: cut, Kind: Half, Op: i}}
	if e.extends[i] {
		out = append(out, Variant{Cut: cut, Kind: Short, Op: i}, Variant{Cut: cut, Kind: Zeros, Op: i})
	}
	return out
}

// Fate is what happened to one operation in a crash state.
type Fate int

const (
	NotIssued Fate = iota
	Kept           // issued and persisted (synced or not)
	Lost           // issued, unsynced, and not persisted
	Torn           // persisted in part
)

// Fate of op i in state v.
func (e *Explorer) Fate(v Variant, i int) (Fate, fsmodel.Torn) {
	if i >= v.Cut {
		return NotIssued, fsmodel.Whole
	}
	if !e.Trace.Ops[i].Mutates() || e.Forced(i, v.Cut) {
		return Kept, fsmodel.Whole
	}
	switch v.Kind {
	case Synced:
		return Lost, fsmodel.Whole
	case Drop:
		if i == v.Op {
			return Lost, fsmodel.Whole
		}
	case Only:
		if i != v.Op {
			return Lost, fsmodel.Whole
		}
	case Half, Short, Zeros:
		if i == v.Op {
			return Torn, map[Kind]fsmodel.Torn{Half: fsmodel.Half, Short: fsmodel.Short, Zeros: fsmodel.Zeros}[v.Kind]
		}
	}
	return Kept, fsmodel.Whole
}

// Valid reports whether v names a state of this trace.
func (e *Explorer) Valid(v Variant) error {
	if v.Cut > len(e.Trace.Ops) {
		return fmt.Errorf("the trace has %d operations; crash point %d is past the end", len(e.Trace.Ops), v.Cut)
	}
	if v.Kind != Prefix && v.Kind != Synced {
		if v.Op >= v.Cut || !e.Trace.Ops[v.Op].Mutates() || e.Forced(v.Op, v.Cut) {
			return fmt.Errorf("op %d is not an unsynced operation at crash point %d", v.Op+1, v.Cut)
		}
		if (v.Kind == Half || v.Kind == Short || v.Kind == Zeros) && e.Trace.Ops[v.Op].Kind != trace.Write {
			return fmt.Errorf("op %d is not a write", v.Op+1)
		}
	}
	return nil
}

// Build returns the file system of crash state v.
func (e *Explorer) Build(v Variant) *fsmodel.FS {
	fs := e.initial.Clone()
	for i := 0; i < v.Cut; i++ {
		o := &e.Trace.Ops[i]
		if !o.Mutates() {
			continue
		}
		switch f, t := e.Fate(v, i); f {
		case Kept:
			fs.Apply(o, fsmodel.Whole)
		case Torn:
			fs.Apply(o, t)
		}
	}
	return fs
}

// Deviation counts how far v is from "everything issued persisted":
// operations lost, plus one for a torn write. Smaller is simpler.
func (e *Explorer) Deviation(v Variant) int {
	n := 0
	for i := 0; i < v.Cut; i++ {
		if f, _ := e.Fate(v, i); f == Lost || f == Torn {
			n++
		}
	}
	return n
}

// Output returns the stdout lines printed before the crash, and the
// acknowledgements among them with the prefix removed.
func (e *Explorer) Output(cut int) (lines, acked []string) {
	for i := 0; i < cut; i++ {
		o := &e.Trace.Ops[i]
		if o.Kind != trace.Out {
			continue
		}
		lines = append(lines, o.Line)
		if o.Ack {
			acked = append(acked, strings.TrimPrefix(o.Line, e.Trace.AckPrefix))
		}
	}
	return
}
