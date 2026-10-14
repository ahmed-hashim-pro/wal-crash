package strace

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"

	"github.com/ahmed-hashim-pro/wal-crash/internal/fsmodel"
	"github.com/ahmed-hashim-pro/wal-crash/internal/trace"
)

// Options for Record.
type Options struct {
	Root      string    // the directory whose durability is under test
	AckPrefix string    // stdout lines starting with this are acknowledgements
	Stdout    io.Writer // where the workload's own output goes (may be nil)
	AllowFail bool      // keep the trace even if the workload exits non-zero
}

// Args are the strace arguments wal-crash records with: follow threads and
// children (-f), print each fd's path (-y), print every byte (-xx) of
// buffers up to 64 MiB (-s), and only the call classes that matter.
func Args(out string) []string {
	return []string{"-f", "-qq", "-e", "signal=none", "-y", "-xx", "-s", "67108864",
		"-e", "trace=%file,%desc,%process,sync", "-o", out}
}

// Record runs argv under strace and returns its trace.
func Record(argv []string, opt Options) (*trace.Trace, error) {
	if len(argv) == 0 {
		return nil, errors.New("no workload command given")
	}
	if _, err := exec.LookPath("strace"); err != nil {
		return nil, errors.New("strace is not installed (wal-crash records with it; try apt install strace)")
	}
	root, err := filepath.Abs(opt.Root)
	if err != nil {
		return nil, err
	}
	if root, err = filepath.EvalSymlinks(root); err != nil {
		return nil, err
	}
	initial, err := Snapshot(root)
	if err != nil {
		return nil, err
	}
	tr := &trace.Trace{Version: trace.Version, Command: argv, Root: root, AckPrefix: opt.AckPrefix, Initial: initial}

	tmp, err := os.CreateTemp("", "wal-crash-strace-*.txt")
	if err != nil {
		return nil, err
	}
	tmp.Close()
	defer os.Remove(tmp.Name())

	cmd := exec.Command("strace", append(append(Args(tmp.Name()), "--"), argv...)...)
	cmd.Stdout = opt.Stdout
	cmd.Stderr = os.Stderr
	runErr := cmd.Run()
	var exit *exec.ExitError
	switch {
	case runErr == nil:
	case errors.As(runErr, &exit) && opt.AllowFail:
		tr.Warnings = append(tr.Warnings, fmt.Sprintf("the workload exited with status %d", exit.ExitCode()))
	case errors.As(runErr, &exit):
		return nil, fmt.Errorf("the workload exited with status %d (wal-crash records only successful runs; see --allow-fail)", exit.ExitCode())
	default:
		return nil, runErr
	}

	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	f, err := os.Open(tmp.Name())
	if err != nil {
		return nil, err
	}
	defer f.Close()
	model, err := parse(f, tr, root, cwd)
	if err != nil {
		return nil, err
	}
	// Self-check: the model's final state should be what is on disk now.
	for _, d := range model.Diff(root) {
		tr.Warnings = append(tr.Warnings, "after the run, "+d)
	}
	return tr, nil
}

func parse(r io.Reader, tr *trace.Trace, root, cwd string) (*fsmodel.FS, error) {
	var model *fsmodel.FS
	err := parseInto(r, tr, root, cwd, &model)
	return model, err
}

// Snapshot reads a directory tree into initial files.
func Snapshot(root string) ([]trace.File, error) {
	var files []trace.File
	err := filepath.WalkDir(root, func(full string, d fs.DirEntry, err error) error {
		if err != nil || full == root {
			return err
		}
		rel, _ := filepath.Rel(root, full)
		switch {
		case d.IsDir():
			files = append(files, trace.File{Path: filepath.ToSlash(rel), Dir: true})
		case d.Type().IsRegular():
			b, err := os.ReadFile(full)
			if err != nil {
				return err
			}
			files = append(files, trace.File{Path: filepath.ToSlash(rel), Data: b})
		default:
			return fmt.Errorf("%s: only regular files and directories are supported", full)
		}
		return nil
	})
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, err
}
