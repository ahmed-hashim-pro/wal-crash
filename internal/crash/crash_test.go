package crash

import (
	"testing"

	"github.com/ahmed-hashim-pro/wal-crash/internal/fsmodel"
	"github.com/ahmed-hashim-pro/wal-crash/internal/trace"
)

const root = fsmodel.Root

// snapshotTrace: write a temp file, fsync it, rename it over "data",
// acknowledge, optionally fsync the directory before acknowledging.
func snapshotTrace(dirSync bool) *trace.Trace {
	ops := []trace.Op{
		{Kind: trace.Create, Dir: root, Name: "tmp", Ino: 3, Path: "tmp"},
		{Kind: trace.Write, Ino: 3, Data: []byte("new"), Path: "tmp"},
		{Kind: trace.Fsync, Ino: 3, Path: "tmp"},
		{Kind: trace.Rename, Dir: root, Name: "tmp", Dir2: root, Name2: "data", Ino: 3, Path: "tmp", To: "data"},
	}
	if dirSync {
		ops = append(ops, trace.Op{Kind: trace.FsyncDir, Ino: root, Path: "."})
	}
	ops = append(ops, trace.Op{Kind: trace.Out, Line: "ACK new", Ack: true})
	return &trace.Trace{Version: trace.Version, AckPrefix: "ACK ", Initial: []trace.File{{Path: "data", Data: []byte("old")}}, Ops: ops}
}

func content(t *testing.T, e *Explorer, v Variant) string {
	t.Helper()
	fs := e.Build(v)
	ino, ok := fs.Lookup("data")
	if !ok {
		return "<none>"
	}
	return string(fs.Inodes[ino].Data)
}

func TestForcing(t *testing.T) {
	e, _ := New(snapshotTrace(false))
	// The write is forced by the fsync (op 2) for crashes after it; the
	// create and rename are never forced: no directory fsync.
	if e.Forced(1, 2) || !e.Forced(1, 3) {
		t.Error("write forcing")
	}
	if e.Forced(0, 5) || e.Forced(3, 5) {
		t.Error("namespace ops forced without a directory fsync")
	}
	e, _ = New(snapshotTrace(true))
	if !e.Forced(3, 5) || !e.Forced(0, 5) {
		t.Error("directory fsync did not force the create and rename")
	}
}

func TestRenameBetweenDirectoriesNeedsBoth(t *testing.T) {
	tr := &trace.Trace{Version: trace.Version, Initial: []trace.File{{Path: "a", Dir: true}, {Path: "b", Dir: true}, {Path: "a/f"}}}
	tr.Ops = []trace.Op{
		// Inodes in sorted path order: a=2, a/f=3, b=4.
		{Kind: trace.Rename, Dir: 2, Name: "f", Dir2: 4, Name2: "f", Ino: 3},
		{Kind: trace.FsyncDir, Ino: 4},
		{Kind: trace.FsyncDir, Ino: 2},
	}
	e, _ := New(tr)
	if e.Forced(0, 2) || !e.Forced(0, 3) {
		t.Error("rename forced before both directories were synced")
	}
}

func TestPOSIXFindsTheLostRename(t *testing.T) {
	e, _ := New(snapshotTrace(false))
	lost := false
	for _, v := range e.Variants(POSIX) {
		if _, acked := e.Output(v.Cut); len(acked) == 1 && content(t, e, v) != "new" {
			lost = true
		}
	}
	if !lost {
		t.Error("posix model never loses the acknowledged rename")
	}
	for _, m := range []Model{Process, Ordered} {
		for _, v := range e.Variants(m) {
			if _, acked := e.Output(v.Cut); len(acked) == 1 && content(t, e, v) != "new" {
				t.Errorf("%s model lost the rename in %s", m, v.ID())
			}
		}
	}
	e, _ = New(snapshotTrace(true))
	for _, v := range e.Variants(POSIX) {
		if _, acked := e.Output(v.Cut); len(acked) == 1 && content(t, e, v) != "new" {
			t.Errorf("with the directory fsync, %s still loses it", v.ID())
		}
	}
}

func TestStatesAreConsistentWithSyncs(t *testing.T) {
	// Whatever the state, a crash after the fsync of the temp file never
	// shows the temp file with torn contents.
	e, _ := New(snapshotTrace(false))
	for _, v := range e.Variants(POSIX) {
		fs := e.Build(v)
		if ino, ok := fs.Lookup("tmp"); ok && v.Cut > 2 && string(fs.Inodes[ino].Data) != "new" {
			t.Errorf("%s: synced file reads %q", v.ID(), fs.Inodes[ino].Data)
		}
	}
}

func TestVariantCounts(t *testing.T) {
	e, _ := New(snapshotTrace(false))
	n := len(e.Trace.Ops)
	if got := len(e.Variants(Process)); got != n+1 {
		t.Errorf("process: %d states, want one per cut point (%d)", got, n+1)
	}
	// ordered adds half/short/zeros for the cut right after the write.
	if got := len(e.Variants(Ordered)); got != n+1+3 {
		t.Errorf("ordered: %d states", got)
	}
	if len(e.Variants(POSIX)) <= len(e.Variants(Ordered)) {
		t.Error("posix explores no more than ordered")
	}
}

func TestIDs(t *testing.T) {
	for _, s := range []string{"c0", "c41", "c41.synced", "c41.drop39", "c41.only1", "c9.half9", "c9.short3", "c9.zeros3"} {
		v, err := ParseID(s)
		if err != nil || v.ID() != s {
			t.Errorf("%s: %v %v", s, v.ID(), err)
		}
	}
	for _, s := range []string{"", "41", "c-1", "c4.drop5", "c4.lose2", "c4.drop", "c4.drop0"} {
		if _, err := ParseID(s); err == nil {
			t.Errorf("%q accepted", s)
		}
	}
	e, _ := New(snapshotTrace(false))
	if err := e.Valid(Variant{Cut: 5, Kind: Drop, Op: 1}); err == nil {
		t.Error("dropping a synced write accepted")
	}
	if err := e.Valid(Variant{Cut: 5, Kind: Drop, Op: 3}); err != nil {
		t.Error(err)
	}
	if err := e.Valid(Variant{Cut: 6}); err == nil {
		t.Error("crash point past the end accepted")
	}
}

func TestDeviation(t *testing.T) {
	e, _ := New(snapshotTrace(false))
	if d := e.Deviation(Variant{Cut: 5}); d != 0 {
		t.Errorf("prefix deviation %d", d)
	}
	if d := e.Deviation(Variant{Cut: 5, Kind: Synced}); d != 2 {
		t.Errorf("synced deviation %d (create and rename lost)", d)
	}
	if d := e.Deviation(Variant{Cut: 5, Kind: Drop, Op: 3}); d != 1 {
		t.Errorf("drop deviation %d", d)
	}
}
