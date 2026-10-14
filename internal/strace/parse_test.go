package strace

import (
	"fmt"
	"strings"
	"testing"

	"github.com/ahmed-hashim-pro/wal-crash/internal/trace"
)

// hex renders a string the way strace -xx does.
func hex(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		fmt.Fprintf(&b, `\x%02x`, s[i])
	}
	return b.String()
}

func q(s string) string         { return `"` + hex(s) + `"` }
func fd(n int, p string) string { return fmt.Sprintf("%d<%s>", n, hex(p)) }

func parseText(t *testing.T, initial []trace.File, lines ...string) *trace.Trace {
	t.Helper()
	tr := &trace.Trace{Version: trace.Version, AckPrefix: "ACK ", Initial: initial}
	if err := Parse(strings.NewReader(strings.Join(lines, "\n")+"\n"), tr, "/r", "/r"); err != nil {
		t.Fatal(err)
	}
	return tr
}

func kinds(tr *trace.Trace) string {
	var s []string
	for _, o := range tr.Ops {
		s = append(s, o.String())
	}
	return strings.Join(s, "\n")
}

func TestParseSnapshotSequence(t *testing.T) {
	tr := parseText(t, nil,
		`5 openat(AT_FDCWD<`+hex("/r")+`>, `+q("data.tmp")+`, O_WRONLY|O_CREAT|O_TRUNC|O_CLOEXEC, 0644) = `+fd(3, "/r/data.tmp"),
		`5 write(`+fd(3, "/r/data.tmp")+`, `+q("abc")+`, 3) = 3`,
		`5 write(`+fd(3, "/r/data.tmp")+`, `+q("de")+`, 2) = 2`,
		`5 fsync(`+fd(3, "/r/data.tmp")+`) = 0`,
		`5 close(`+fd(3, "/r/data.tmp")+`) = 0`,
		`5 renameat(AT_FDCWD<`+hex("/r")+`>, `+q("data.tmp")+`, AT_FDCWD<`+hex("/r")+`>, `+q("data")+`) = 0`,
		`5 openat(AT_FDCWD<`+hex("/r")+`>, `+q(".")+`, O_RDONLY|O_CLOEXEC) = `+fd(4, "/r"),
		`5 fsync(`+fd(4, "/r")+`) = 0`,
		`5 write(1<`+hex("pipe:[1]")+`>, `+q("ACK k1 v1\nPU")+`, 12) = 12`,
		`5 write(1<`+hex("pipe:[1]")+`>, `+q("T k2\n")+`, 5) = 5`,
	)
	want := strings.Join([]string{
		"create   data.tmp",
		"write    data.tmp  3 B @ 0",
		"write    data.tmp  2 B @ 3",
		"fsync    data.tmp",
		"rename   data.tmp -> data",
		"fsync    ./  (directory)",
		"stdout   ACK k1 v1",
		"stdout   PUT k2",
	}, "\n")
	if got := kinds(tr); got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
	if !tr.Ops[6].Ack || tr.Ops[7].Ack {
		t.Error("ack flags wrong")
	}
	if tr.Ops[4].Ino != tr.Ops[0].Ino {
		t.Error("rename moved a different inode than the one created")
	}
}

func TestParseAppendPositionsAndDup(t *testing.T) {
	tr := parseText(t, []trace.File{{Path: "log", Data: []byte("0123")}},
		`5 openat(AT_FDCWD, `+q("/r/log")+`, O_WRONLY|O_APPEND) = `+fd(3, "/r/log"),
		`5 write(`+fd(3, "/r/log")+`, `+q("xy")+`, 2) = 2`,
		`5 openat(AT_FDCWD, `+q("/r/log")+`, O_RDWR) = `+fd(4, "/r/log"),
		`5 read(`+fd(4, "/r/log")+`, `+q("01")+`, 2) = 2`,
		`5 dup2(`+fd(4, "/r/log")+`, 9) = `+fd(9, "/r/log"),
		`5 write(`+fd(9, "/r/log")+`, `+q("Z")+`, 1) = 1`,
		`5 write(`+fd(4, "/r/log")+`, `+q("Q")+`, 1) = 1`,
		`5 lseek(`+fd(4, "/r/log")+`, 0, SEEK_SET) = 0`,
		`5 write(`+fd(4, "/r/log")+`, `+q("A")+`, 1) = 1`,
		`5 pwrite64(`+fd(4, "/r/log")+`, `+q("P")+`, 1, 5) = 1`,
	)
	var offs []int64
	for _, o := range tr.Ops {
		offs = append(offs, o.Off)
	}
	// append goes to the end (4); the dup shares fd 4's position (2, then 3);
	// lseek resets it; pwrite uses its own offset.
	if fmt.Sprint(offs) != "[4 2 3 0 5]" {
		t.Errorf("offsets %v", offs)
	}
}

func TestParseThreadsShareAndForksCopy(t *testing.T) {
	tr := parseText(t, nil,
		`5 openat(AT_FDCWD, `+q("/r/a")+`, O_WRONLY|O_CREAT, 0644) = `+fd(3, "/r/a"),
		// The thread prints before strace prints clone returning.
		`6 write(`+fd(3, "/r/a")+`, `+q("t")+`, 1) = 1`,
		`5 clone(child_stack=0x1, flags=CLONE_VM|CLONE_FS|CLONE_FILES|CLONE_SIGHAND|CLONE_THREAD) = 6`,
		`5 write(`+fd(3, "/r/a")+`, `+q("p")+`, 1) = 1`,
		`5 fork() = 7`,
		`7 lseek(`+fd(3, "/r/a")+`, 0, SEEK_SET) = 0`,
		`7 close(`+fd(3, "/r/a")+`) = 0`,
		`5 write(`+fd(3, "/r/a")+`, `+q("q")+`, 1) = 1`,
	)
	var offs []int64
	for _, o := range tr.Ops[1:] {
		offs = append(offs, o.Off)
	}
	// The child's lseek moves the shared position; its close does not
	// close the parent's descriptor.
	if fmt.Sprint(offs) != "[0 1 0]" {
		t.Errorf("offsets %v", offs)
	}
}

func TestParseNamespace(t *testing.T) {
	tr := parseText(t, []trace.File{{Path: "old", Data: []byte("x")}},
		`5 mkdir(`+q("/r/sub")+`, 0755) = 0`,
		`5 openat(AT_FDCWD, `+q("/r/sub/f")+`, O_WRONLY|O_CREAT|O_TRUNC, 0644) = `+fd(3, "/r/sub/f"),
		`5 rename(`+q("/r/sub/f")+`, `+q("/r/g")+`) = 0`,
		`5 unlink(`+q("/r/old")+`) = 0`,
		`5 openat(AT_FDCWD, `+q("/r/g")+`, O_WRONLY|O_TRUNC) = `+fd(4, "/r/g"),
		`5 unlinkat(AT_FDCWD, `+q("/r/sub")+`, AT_REMOVEDIR) = 0`,
		`5 openat(AT_FDCWD, `+q("/elsewhere")+`, O_WRONLY|O_CREAT, 0644) = `+fd(5, "/elsewhere"),
		`5 write(`+fd(5, "/elsewhere")+`, `+q("ignored")+`, 7) = 7`,
		`5 openat(AT_FDCWD, `+q("/r/missing")+`, O_RDONLY) = -1 ENOENT (No such file or directory)`,
		`5 sync() = 0`,
	)
	want := strings.Join([]string{
		"mkdir    sub",
		"create   sub/f",
		"rename   sub/f -> g",
		"unlink   old",
		"rmdir    sub",
		"sync",
	}, "\n")
	if got := kinds(tr); got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
}

func TestParseRejectsWhatItCannotModel(t *testing.T) {
	for name, line := range map[string]string{
		"rename out":   `5 rename(` + q("/r/a") + `, ` + q("/tmp/a") + `) = 0`,
		"link":         `5 link(` + q("/r/a") + `, ` + q("/r/b") + `) = 0`,
		"exchange":     `5 renameat2(AT_FDCWD, ` + q("/r/a") + `, AT_FDCWD, ` + q("/r/b") + `, RENAME_EXCHANGE) = 0`,
		"unknown file": `5 openat(AT_FDCWD, ` + q("/r/ghost") + `, O_RDONLY) = ` + fd(3, "/r/ghost"),
	} {
		tr := &trace.Trace{Version: trace.Version, Initial: []trace.File{{Path: "a"}, {Path: "b"}}}
		if err := Parse(strings.NewReader(line+"\n"), tr, "/r", "/r"); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestParseWarnsAboutSharedMappings(t *testing.T) {
	tr := parseText(t, []trace.File{{Path: "db-shm"}},
		`5 openat(AT_FDCWD, `+q("/r/db-shm")+`, O_RDWR) = `+fd(3, "/r/db-shm"),
		`5 mmap(NULL, 32768, PROT_READ|PROT_WRITE, MAP_SHARED, `+fd(3, "/r/db-shm")+`, 0) = 0x7f0000000000`,
	)
	if len(tr.Warnings) != 1 || !strings.Contains(tr.Warnings[0], "db-shm is mapped shared") {
		t.Errorf("warnings %q", tr.Warnings)
	}
}

func TestParseWritev(t *testing.T) {
	tr := parseText(t, nil,
		`5 openat(AT_FDCWD, `+q("/r/a")+`, O_WRONLY|O_CREAT, 0644) = `+fd(3, "/r/a"),
		`5 writev(`+fd(3, "/r/a")+`, [{iov_base=`+q("ab")+`, iov_len=2}, {iov_base=`+q("cd")+`, iov_len=2}], 2) = 3`,
	)
	if string(tr.Ops[1].Data) != "abc" {
		t.Errorf("writev data %q", tr.Ops[1].Data)
	}
}
