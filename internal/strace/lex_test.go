package strace

import (
	"reflect"
	"testing"
)

func TestUnquote(t *testing.T) {
	for _, c := range []struct {
		in    string
		want  string
		trunc bool
	}{
		{`"\x68\x69\x0a"`, "hi\n", false},
		{`"a\"b\\c"`, `a"b\c`, false},
		{`"\n\t\0\177"`, "\n\t\x00\x7f", false},
		{`"\x41\x42"...`, "AB", true},
		{`""`, "", false},
	} {
		got, trunc, err := unquote(c.in)
		if err != nil || string(got) != c.want || trunc != c.trunc {
			t.Errorf("unquote(%s) = %q, %v, %v; want %q, %v", c.in, got, trunc, err, c.want, c.trunc)
		}
	}
	if _, _, err := unquote(`"\x4"`); err == nil {
		t.Error("short escape accepted")
	}
}

func TestFdArg(t *testing.T) {
	n, p := fdArg(`3<\x2f\x74\x6d\x70\x2f\x78>`)
	if n != 3 || p != "/tmp/x" {
		t.Errorf("got %d %q", n, p)
	}
	n, p = fdArg(`AT_FDCWD<\x2f\x77>`)
	if n != -100 || p != "/w" {
		t.Errorf("got %d %q", n, p)
	}
	if n, p = fdArg("7"); n != 7 || p != "" {
		t.Errorf("got %d %q", n, p)
	}
}

func TestSplitArgs(t *testing.T) {
	args, rest, err := splitArgs(`3<\x2f\x61>, [{iov_base="\x29\x2c", iov_len=2}], 1) = 2`)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{`3<\x2f\x61>`, `[{iov_base="\x29\x2c", iov_len=2}]`, "1"}
	if !reflect.DeepEqual(args, want) || rest != " = 2" {
		t.Errorf("got %q %q", args, rest)
	}
	if args, _, _ := splitArgs(`) = 0`); len(args) != 0 {
		t.Errorf("sync() parsed as %q", args)
	}
}

func TestLineParserJoinsInterruptedCalls(t *testing.T) {
	lp := &lineParser{pending: map[int]string{}}
	lines := []string{
		`10    write(1<\x70\x69\x70\x65>, "\x68\x69", 2 <unfinished ...>`,
		`11    fsync(3<\x2f\x61>) = 0`,
		`10    <... write resumed>)              = 2`,
		`10    +++ exited with 0 +++`,
	}
	var got []*call
	for i, l := range lines {
		c, err := lp.parse(l, i+1)
		if err != nil {
			t.Fatal(err)
		}
		if c != nil {
			got = append(got, c)
		}
	}
	if len(got) != 2 || got[0].name != "fsync" || got[1].name != "write" || got[1].retInt() != 2 || got[1].pid != 10 {
		t.Fatalf("got %+v", got)
	}
	if got[1].line != 3 {
		t.Errorf("a joined call is placed where it finished: line %d", got[1].line)
	}
}

func TestReturnValues(t *testing.T) {
	c := &call{ret: `3<\x2f\x74\x2f\x61>`}
	if c.retInt() != 3 || c.retPath() != "/t/a" {
		t.Errorf("got %d %q", c.retInt(), c.retPath())
	}
	c = &call{ret: "-1 ENOENT (No such file or directory)"}
	if c.retInt() != -1 {
		t.Errorf("got %d", c.retInt())
	}
}
