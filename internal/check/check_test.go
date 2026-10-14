package check

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ahmed-hashim-pro/wal-crash/internal/crash"
	"github.com/ahmed-hashim-pro/wal-crash/internal/fsmodel"
	"github.com/ahmed-hashim-pro/wal-crash/internal/trace"
)

func renameTrace() *trace.Trace {
	r := fsmodel.Root
	return &trace.Trace{Version: trace.Version, AckPrefix: "ACK ",
		Initial: []trace.File{{Path: "data", Data: []byte("old\n")}},
		Ops: []trace.Op{
			{Kind: trace.Create, Dir: r, Name: "tmp", Ino: 3, Path: "tmp"},
			{Kind: trace.Write, Ino: 3, Data: []byte("new\n"), Path: "tmp"},
			{Kind: trace.Fsync, Ino: 3, Path: "tmp"},
			{Kind: trace.Rename, Dir: r, Name: "tmp", Dir2: r, Name2: "data", Ino: 3, Path: "tmp", To: "data"},
			{Kind: trace.Out, Line: "ACK new", Ack: true},
		}}
}

// The checker: every acknowledged value must be the content of "data".
const checker = `while read -r v; do grep -qx "$v" data || { echo "lost $v"; exit 1; }; done < "$WAL_CRASH_ACKED"`

func TestRunFindsTheSimplestViolation(t *testing.T) {
	e, _ := crash.New(renameTrace())
	r, err := Run(e, crash.POSIX, Options{Checker: checker, Jobs: 2, Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Violations) == 0 {
		t.Fatal("no violation found")
	}
	v := r.Violations[0]
	if v.ID != "c5.drop4" || v.Output != "lost new" {
		t.Errorf("simplest violation %s %q", v.ID, v.Output)
	}
	var out strings.Builder
	r.Print(&out, e, "t.json")
	for _, want := range []string{"FAIL", "✗", "rename   tmp -> data", "LOST: directory not fsynced", "wal-crash state t.json c5.drop4"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("report lacks %q:\n%s", want, out.String())
		}
	}

	r, err = Run(e, crash.Process, Options{Checker: checker})
	if err != nil || len(r.Violations) != 0 {
		t.Errorf("process model: %v %v", err, r.Violations)
	}
}

func TestRunDeduplicates(t *testing.T) {
	e, _ := crash.New(renameTrace())
	r, err := Run(e, crash.POSIX, Options{Checker: "true"})
	if err != nil {
		t.Fatal(err)
	}
	if r.Distinct >= r.States {
		t.Errorf("%d states, %d distinct: nothing deduplicated", r.States, r.Distinct)
	}
}

func TestBrokenCheckerFailsFast(t *testing.T) {
	e, _ := crash.New(renameTrace())
	_, err := Run(e, crash.POSIX, Options{Checker: "no-such-command-xyz"})
	if !errors.Is(err, ErrChecker) {
		t.Errorf("got %v", err)
	}
}

func TestTimeoutIsAViolation(t *testing.T) {
	e, _ := crash.New(renameTrace())
	r, err := Run(e, crash.Process, Options{Checker: "sleep 5", Timeout: 200 * time.Millisecond, Jobs: 8})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Violations) != r.Distinct || !strings.Contains(r.Violations[0].Output, "timed out") {
		t.Errorf("timeouts not reported: %+v", r.Violations)
	}
}

func TestShape(t *testing.T) {
	if Shape("row k12 is missing (11)\nmore") != "row k# is missing (#)" {
		t.Error(Shape("row k12 is missing (11)"))
	}
}
