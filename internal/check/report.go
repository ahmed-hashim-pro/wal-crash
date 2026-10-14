package check

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/ahmed-hashim-pro/wal-crash/internal/crash"
	"github.com/ahmed-hashim-pro/wal-crash/internal/fsmodel"
	"github.com/ahmed-hashim-pro/wal-crash/internal/trace"
)

// Summary is the one-line verdict.
func (r *Report) Summary() string {
	verdict := "PASS"
	if len(r.Violations) > 0 {
		verdict = fmt.Sprintf("FAIL  %d of them break the checker", len(r.Violations))
	}
	return fmt.Sprintf("%-8s %s states (%s distinct) checked   %s", r.Model, comma(r.States), comma(r.Distinct), verdict)
}

// Print writes the full report: the summary, the simplest violation with
// the operations around it, and the other kinds of failure seen.
func (r *Report) Print(w io.Writer, e *crash.Explorer, tracePath string) {
	fmt.Fprintf(w, "%s\n", r.Summary())
	if len(r.Violations) == 0 {
		return
	}
	v := r.Violations[0]
	fmt.Fprintln(w)
	PrintState(w, e, v.Variant)
	fmt.Fprintf(w, "\n  checker: %s\n", indent(v.Output, "           "))
	if tracePath != "" {
		fmt.Fprintf(w, "\n  reproduce: wal-crash state %s %s -o /tmp/state\n", tracePath, v.ID)
	}
	// The rest, grouped by message with the numbers taken out, so
	// "row k2 is missing" and "row k3 is missing" count as one kind.
	type group struct {
		msg, first string
		n          int
	}
	same := 0
	seen := map[string]*group{}
	var groups []*group
	for _, x := range r.Violations[1:] {
		k := Shape(x.Output)
		if k == Shape(v.Output) {
			same++
			continue
		}
		g := seen[k]
		if g == nil {
			g = &group{msg: firstLine(x.Output), first: x.ID}
			seen[k] = g
			groups = append(groups, g)
		}
		g.n++
	}
	if same > 0 {
		fmt.Fprintf(w, "\n  %d more states fail the same way\n", same)
	}
	if len(groups) > 0 {
		sort.SliceStable(groups, func(i, j int) bool { return groups[i].n > groups[j].n })
		fmt.Fprintf(w, "\n  other kinds of failure:\n")
		for i, g := range groups {
			if i == 5 {
				fmt.Fprintf(w, "    ... and %d more kinds\n", len(groups)-5)
				break
			}
			fmt.Fprintf(w, "    %5d like: %s  (e.g. %s)\n", g.n, g.msg, g.first)
		}
	}
}

// Shape is a failure message's first line with every run of digits
// replaced by "#".
func Shape(msg string) string {
	var b strings.Builder
	digits := false
	for _, c := range firstLine(msg) {
		if c >= '0' && c <= '9' {
			if !digits {
				b.WriteByte('#')
			}
			digits = true
			continue
		}
		digits = false
		b.WriteRune(c)
	}
	return b.String()
}

// PrintState describes a crash state: where the crash was and what was
// lost, with the operations around it.
func PrintState(w io.Writer, e *crash.Explorer, v crash.Variant) {
	ops := e.Trace.Ops
	var dev []int
	for i := 0; i < v.Cut; i++ {
		if f, _ := e.Fate(v, i); f == crash.Lost || f == crash.Torn {
			dev = append(dev, i)
		}
	}
	where := "before the first operation"
	if v.Cut > 0 {
		where = fmt.Sprintf("after op %d of %d", v.Cut, len(ops))
	}
	what := "everything issued before it reached the disk"
	switch n := len(dev); {
	case v.Kind == crash.Half || v.Kind == crash.Short || v.Kind == crash.Zeros:
		what = "one write torn"
	case n == 1:
		what = "1 operation lost"
	case n > 1:
		what = fmt.Sprintf("%d operations lost", n)
	}
	fmt.Fprintf(w, "  crash %s (state %s), %s:\n\n", where, v.ID(), what)

	show := map[int]bool{}
	for _, i := range dev {
		for j := i - 2; j <= i; j++ {
			show[j] = true
		}
	}
	for j := v.Cut - 4; j < v.Cut; j++ {
		show[j] = true
	}
	last := -1
	for i := 0; i < v.Cut; i++ {
		if !show[i] {
			continue
		}
		if last >= 0 && i > last+1 {
			fmt.Fprintf(w, "         ...\n")
		}
		last = i
		o := &ops[i]
		mark, note := "   ", ""
		switch f, t := e.Fate(v, i); f {
		case crash.Lost:
			mark, note = " ✗ ", "LOST: "+why(e, o)
		case crash.Torn:
			mark, note = " ~ ", "TORN: "+tornNote(t, len(o.Data))
		}
		desc := o.String()
		if len(desc) > 52 {
			desc = desc[:51] + "…"
		}
		if note != "" {
			fmt.Fprintf(w, "  %s%4d  %-52s  %s\n", mark, i+1, desc, note)
		} else {
			fmt.Fprintf(w, "  %s%4d  %s\n", mark, i+1, desc)
		}
	}
}

func why(e *crash.Explorer, o *trace.Op) string {
	switch {
	case o.Kind == trace.Write || o.Kind == trace.Truncate:
		return "file not fsynced after it"
	case o.Kind == trace.Rename && o.Dir2 != o.Dir:
		return "both directories not fsynced after it"
	default:
		return "directory not fsynced after it"
	}
}

func tornNote(t fsmodel.Torn, n int) string {
	h := fsmodel.Split(n)
	switch t {
	case fsmodel.Half:
		return fmt.Sprintf("only the first %d of %d bytes written", h, n)
	case fsmodel.Short:
		return fmt.Sprintf("only the first %d of %d bytes, file ends there", h, n)
	case fsmodel.Zeros:
		return "file grew but the bytes read as zeros"
	}
	return ""
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(s, "\n")
	return s
}

func indent(s, pad string) string {
	return strings.ReplaceAll(s, "\n", "\n"+pad)
}

func comma(n int) string {
	s := fmt.Sprint(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}
