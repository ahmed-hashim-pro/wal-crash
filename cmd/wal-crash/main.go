// Command wal-crash records what a program does to a directory and checks
// that it recovers from every crash state a power failure may leave.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/ahmed-hashim-pro/wal-crash/internal/check"
	"github.com/ahmed-hashim-pro/wal-crash/internal/crash"
	"github.com/ahmed-hashim-pro/wal-crash/internal/strace"
	"github.com/ahmed-hashim-pro/wal-crash/internal/trace"
)

const usage = `wal-crash: find out what your program loses when the power goes.

  wal-crash run    [flags] -- COMMAND...   record COMMAND, then check every crash state
  wal-crash record [flags] -- COMMAND...   only record, to a trace file
  wal-crash check  [flags] TRACE           check a recorded trace
  wal-crash ops    TRACE                   list the recorded operations
  wal-crash state  TRACE ID -o DIR         write one crash state out to look at

COMMAND works on one directory (-d DIR, or a fresh empty one wal-crash makes;
"{dir}" in COMMAND is replaced by its path). Lines it prints starting with
"ACK " are promises: after a crash, the checker decides whether they were kept.
The checker is a shell command run inside each crash state's directory, with
$WAL_CRASH_ACKED naming a file of the acknowledged lines (prefix removed) and
$WAL_CRASH_STDOUT a file of everything printed before the crash. It exits 0
if the state is acceptable, non-zero (saying why) if not.

Models (--model):
  process  the program is killed; everything it wrote survives, in order
  ordered  power fails; the disk wrote in order, possibly half of the last write
  posix    power fails; only what fsync promised survives (default)
  all      check all three

Run "wal-crash COMMAND -h" for a command's flags.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	code := 0
	switch os.Args[1] {
	case "run":
		code, err = cmdRun(os.Args[2:])
	case "record":
		err = cmdRecord(os.Args[2:])
	case "check":
		code, err = cmdCheck(os.Args[2:])
	case "ops":
		err = cmdOps(os.Args[2:])
	case "state":
		err = cmdState(os.Args[2:])
	case "-h", "--help", "help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "wal-crash: unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "wal-crash:", err)
		os.Exit(2)
	}
	os.Exit(code)
}

type recordFlags struct {
	dir       string
	ack       string
	allowFail bool
	verbose   bool
}

func (r *recordFlags) add(fs *flag.FlagSet) {
	fs.StringVar(&r.dir, "d", "", "the directory under test (default: a fresh empty one, substituted for {dir})")
	fs.StringVar(&r.ack, "ack", "ACK ", "prefix of acknowledgement lines on stdout")
	fs.BoolVar(&r.allowFail, "allow-fail", false, "keep the trace even if COMMAND exits non-zero")
	fs.BoolVar(&r.verbose, "v", false, "show COMMAND's own output")
}

func (r *recordFlags) record(argv []string) (*trace.Trace, func(), error) {
	cleanup := func() {}
	dir := r.dir
	if dir == "" {
		d, err := os.MkdirTemp("", "wal-crash-run-*")
		if err != nil {
			return nil, cleanup, err
		}
		dir, cleanup = d, func() { os.RemoveAll(d) }
	}
	argv = append([]string(nil), argv...)
	for i := range argv {
		argv[i] = strings.ReplaceAll(argv[i], "{dir}", dir)
	}
	os.Setenv("WAL_CRASH_DIR", dir)
	var out io.Writer
	if r.verbose {
		out = os.Stdout
	}
	tr, err := strace.Record(argv, strace.Options{Root: dir, AckPrefix: r.ack, Stdout: out, AllowFail: r.allowFail})
	return tr, cleanup, err
}

type checkFlags struct {
	model   string
	checker string
	jobs    int
	timeout time.Duration
	json    bool
}

func (c *checkFlags) add(fs *flag.FlagSet) {
	fs.StringVar(&c.model, "model", "posix", "process, ordered, posix, or all")
	fs.StringVar(&c.checker, "checker", "", "shell command that judges one crash state (required)")
	fs.IntVar(&c.jobs, "j", 0, "checkers to run in parallel (default: number of CPUs)")
	fs.DurationVar(&c.timeout, "timeout", 30*time.Second, "time limit for one check")
	fs.BoolVar(&c.json, "json", false, "print the report as JSON")
}

func (c *checkFlags) run(tr *trace.Trace, tracePath string) (int, error) {
	if c.checker == "" {
		return 0, errors.New("--checker is required")
	}
	models := crash.Models
	if c.model != "all" {
		m, err := crash.ParseModel(c.model)
		if err != nil {
			return 0, err
		}
		models = []crash.Model{m}
	}
	e, err := crash.New(tr)
	if err != nil {
		return 0, err
	}
	if !c.json {
		fmt.Printf("%d operations recorded, %d acknowledged\n", len(tr.Ops), len(tr.Acks()))
		for _, w := range tr.Warnings {
			fmt.Printf("warning: %s\n", w)
		}
		fmt.Println()
	}
	var reports []*check.Report
	code := 0
	for _, m := range models {
		r, err := check.Run(e, m, check.Options{Checker: c.checker, Jobs: c.jobs, Timeout: c.timeout})
		if err != nil {
			return 0, err
		}
		reports = append(reports, r)
		if len(r.Violations) > 0 {
			code = 1
		}
	}
	if c.json {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return code, enc.Encode(reports)
	}
	if len(reports) == 1 {
		reports[0].Print(os.Stdout, e, tracePath)
		return code, nil
	}
	for _, r := range reports {
		fmt.Println(r.Summary())
	}
	for _, r := range reports {
		if len(r.Violations) > 0 {
			fmt.Printf("\nsimplest failure under the %s model:\n\n", r.Model)
			check.PrintState(os.Stdout, e, r.Violations[0].Variant)
			fmt.Printf("\n  checker: %s\n", r.Violations[0].Output)
			if tracePath != "" {
				fmt.Printf("\n  reproduce: wal-crash state %s %s -o /tmp/state\n", tracePath, r.Violations[0].ID)
			}
			break
		}
	}
	return code, nil
}

func cmdRun(args []string) (int, error) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	var rf recordFlags
	var cf checkFlags
	rf.add(fs)
	cf.add(fs)
	save := fs.String("o", "", "also save the trace to this file")
	fs.Parse(args)
	if cf.checker == "" {
		return 0, errors.New("--checker is required")
	}
	tr, cleanup, err := rf.record(fs.Args())
	defer cleanup()
	if err != nil {
		return 0, err
	}
	if *save != "" {
		if err := tr.Save(*save); err != nil {
			return 0, err
		}
	}
	return cf.run(tr, *save)
}

func cmdRecord(args []string) error {
	fs := flag.NewFlagSet("record", flag.ExitOnError)
	var rf recordFlags
	rf.add(fs)
	out := fs.String("o", "trace.json", "trace file to write")
	fs.Parse(args)
	tr, cleanup, err := rf.record(fs.Args())
	defer cleanup()
	if err != nil {
		return err
	}
	for _, w := range tr.Warnings {
		fmt.Fprintf(os.Stderr, "warning: %s\n", w)
	}
	fmt.Printf("recorded %d operations (%d acknowledged) to %s\n", len(tr.Ops), len(tr.Acks()), *out)
	return tr.Save(*out)
}

func cmdCheck(args []string) (int, error) {
	fs := flag.NewFlagSet("check", flag.ExitOnError)
	var cf checkFlags
	cf.add(fs)
	fs.Parse(reorder(args))
	if fs.NArg() != 1 {
		return 0, errors.New("usage: wal-crash check [flags] TRACE")
	}
	tr, err := trace.Load(fs.Arg(0))
	if err != nil {
		return 0, err
	}
	return cf.run(tr, fs.Arg(0))
}

func cmdOps(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: wal-crash ops TRACE")
	}
	tr, err := trace.Load(args[0])
	if err != nil {
		return err
	}
	e, err := crash.New(tr)
	if err != nil {
		return err
	}
	fmt.Printf("%s\nin %s\n\n", strings.Join(tr.Command, " "), tr.Root)
	for i := range tr.Ops {
		o := &tr.Ops[i]
		note := ""
		if o.Mutates() {
			if !e.Forced(i, len(tr.Ops)) {
				note = "  (never synced)"
			}
		}
		fmt.Printf("%5d  %s%s\n", i+1, o, note)
	}
	return nil
}

func cmdState(args []string) error {
	fs := flag.NewFlagSet("state", flag.ExitOnError)
	out := fs.String("o", "", "directory to create (required)")
	fs.Parse(reorder(args))
	if fs.NArg() != 2 || *out == "" {
		return errors.New("usage: wal-crash state TRACE ID -o DIR")
	}
	tr, err := trace.Load(fs.Arg(0))
	if err != nil {
		return err
	}
	v, err := crash.ParseID(fs.Arg(1))
	if err != nil {
		return err
	}
	e, err := crash.New(tr)
	if err != nil {
		return err
	}
	if err := e.Valid(v); err != nil {
		return err
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		return err
	}
	state, err := check.Materialize(e, v, *out)
	if err != nil {
		return err
	}
	check.PrintState(os.Stdout, e, v)
	fmt.Printf("\nfiles:  %s\nrun your checker there with\n  WAL_CRASH_ACKED=%s/acked WAL_CRASH_STDOUT=%s/stdout\n", state, *out, *out)
	return nil
}

// reorder moves flags before positional arguments, so "check trace.json
// --checker X" works as well as "check --checker X trace.json".
func reorder(args []string) []string {
	var flags, pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") && len(a) > 1 {
			flags = append(flags, a)
			if !strings.Contains(a, "=") && i+1 < len(args) && !isBool(a) {
				flags = append(flags, args[i+1])
				i++
			}
			continue
		}
		pos = append(pos, a)
	}
	return append(flags, pos...)
}

func isBool(a string) bool {
	a = strings.TrimLeft(a, "-")
	return a == "json"
}
