// Package check materializes crash states, runs the user's checker on each
// distinct one, and reports what broke.
package check

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ahmed-hashim-pro/wal-crash/internal/crash"
	"github.com/ahmed-hashim-pro/wal-crash/internal/fsmodel"
)

type Options struct {
	Checker string        // shell command, run in the crash state's directory
	Jobs    int           // parallel checkers (default: CPUs)
	Timeout time.Duration // per check
	WorkDir string        // where states are materialized (default: temp)
}

// Result of checking one distinct state.
type Result struct {
	Variant crash.Variant `json:"-"`
	ID      string        `json:"id"`
	OK      bool          `json:"ok"`
	Output  string        `json:"output,omitempty"`
	Same    int           `json:"same"` // how many explored states had these exact contents and output
}

type Report struct {
	Model      crash.Model `json:"model"`
	Ops        int         `json:"ops"`
	Acks       int         `json:"acks"`
	States     int         `json:"states"`
	Distinct   int         `json:"distinct"`
	Violations []Result    `json:"violations"`
	Elapsed    string      `json:"elapsed"`
	Jobs       int         `json:"jobs"`
}

// ErrChecker means the checker could not be run at all.
var ErrChecker = errors.New("checker did not run")

func Run(e *crash.Explorer, m crash.Model, opt Options) (*Report, error) {
	start := time.Now()
	if opt.Jobs <= 0 {
		opt.Jobs = runtime.NumCPU()
	}
	if opt.Timeout <= 0 {
		opt.Timeout = 30 * time.Second
	}
	work := opt.WorkDir
	if work == "" {
		d, err := os.MkdirTemp("", "wal-crash-states-*")
		if err != nil {
			return nil, err
		}
		defer os.RemoveAll(d)
		work = d
	}

	// Distinct states: the same file contents with the same output before
	// the crash will get the same verdict, so each is checked once.
	type key struct {
		h        [32]byte
		out, ack int
	}
	variants := e.Variants(m)
	index := map[key]int{}
	var distinct []Result
	for _, v := range variants {
		lines, acked := e.Output(v.Cut)
		k := key{e.Build(v).Hash(), len(lines), len(acked)}
		if i, ok := index[k]; ok {
			distinct[i].Same++
			// Keep the simplest representative.
			if better(e, v, distinct[i].Variant) {
				distinct[i].Variant, distinct[i].ID = v, v.ID()
			}
			continue
		}
		index[k] = len(distinct)
		distinct = append(distinct, Result{Variant: v, ID: v.ID(), Same: 1})
	}

	// The first state is checked alone, so a checker that cannot run at
	// all fails fast instead of thousands of times.
	if err := checkOne(e, &distinct[0], opt, work); err != nil {
		return nil, err
	}
	var wg sync.WaitGroup
	next := make(chan int)
	errs := make(chan error, opt.Jobs)
	for w := 0; w < opt.Jobs; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				if err := checkOne(e, &distinct[i], opt, work); err != nil {
					select {
					case errs <- err:
					default:
					}
				}
			}
		}()
	}
	for i := 1; i < len(distinct); i++ {
		next <- i
	}
	close(next)
	wg.Wait()
	select {
	case err := <-errs:
		return nil, err
	default:
	}

	r := &Report{Model: m, Ops: len(e.Trace.Ops), Acks: len(e.Trace.Acks()), States: len(variants),
		Distinct: len(distinct), Jobs: opt.Jobs}
	for _, d := range distinct {
		if !d.OK {
			r.Violations = append(r.Violations, d)
		}
	}
	sort.SliceStable(r.Violations, func(i, j int) bool {
		return better(e, r.Violations[i].Variant, r.Violations[j].Variant)
	})
	r.Elapsed = time.Since(start).Round(10 * time.Millisecond).String()
	return r, nil
}

// better orders states by simplicity: fewest deviations from "everything
// issued persisted", then earliest crash, then whole operations lost
// before writes torn.
func better(e *crash.Explorer, a, b crash.Variant) bool {
	da, db := e.Deviation(a), e.Deviation(b)
	if da != db {
		return da < db
	}
	if a.Cut != b.Cut {
		return a.Cut < b.Cut
	}
	if ra, rb := rank[a.Kind], rank[b.Kind]; ra != rb {
		return ra < rb
	}
	return a.ID() < b.ID()
}

var rank = map[crash.Kind]int{crash.Prefix: 0, crash.Synced: 1, crash.Drop: 1, crash.Only: 2,
	crash.Half: 3, crash.Short: 3, crash.Zeros: 3}

func checkOne(e *crash.Explorer, r *Result, opt Options, work string) error {
	dir, err := os.MkdirTemp(work, "s-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	state, err := Materialize(e, r.Variant, dir)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), opt.Timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", opt.Checker)
	cmd.Dir = state
	cmd.Env = append(os.Environ(),
		"WAL_CRASH_DIR="+state,
		"WAL_CRASH_ACKED="+filepath.Join(dir, "acked"),
		"WAL_CRASH_STDOUT="+filepath.Join(dir, "stdout"),
		"WAL_CRASH_STATE="+r.Variant.ID())
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err = cmd.Run()
	r.Output = strings.TrimSpace(limit(out.String(), 4000))
	var exit *exec.ExitError
	switch {
	case err == nil:
		r.OK = true
	case ctx.Err() != nil:
		r.Output = fmt.Sprintf("checker timed out after %s", opt.Timeout)
	case errors.As(err, &exit) && (exit.ExitCode() == 126 || exit.ExitCode() == 127):
		return fmt.Errorf("%w: %q exited %d: %s", ErrChecker, opt.Checker, exit.ExitCode(), r.Output)
	case errors.As(err, &exit):
		if r.Output == "" {
			r.Output = fmt.Sprintf("checker exited %d", exit.ExitCode())
		}
	default:
		return fmt.Errorf("%w: %v", ErrChecker, err)
	}
	return nil
}

// Materialize writes state v under dir: the files in dir/state, the
// acknowledged lines in dir/acked and all output lines in dir/stdout. It
// returns the state directory.
func Materialize(e *crash.Explorer, v crash.Variant, dir string) (string, error) {
	state := filepath.Join(dir, "state")
	if err := os.Mkdir(state, 0o755); err != nil {
		return "", err
	}
	if err := e.Build(v).Materialize(state); err != nil {
		return "", err
	}
	lines, acked := e.Output(v.Cut)
	if err := os.WriteFile(filepath.Join(dir, "acked"), joinLines(acked), 0o644); err != nil {
		return "", err
	}
	return state, os.WriteFile(filepath.Join(dir, "stdout"), joinLines(lines), 0o644)
}

func joinLines(l []string) []byte {
	if len(l) == 0 {
		return nil
	}
	return []byte(strings.Join(l, "\n") + "\n")
}

func limit(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// Build is exported for the state command.
func Build(e *crash.Explorer, v crash.Variant) *fsmodel.FS { return e.Build(v) }
