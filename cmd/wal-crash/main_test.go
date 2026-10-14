package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// These tests drive the real binary end to end. They need strace, so they
// are skipped where it is missing (CI runs them in the dev image).

var bin, kv string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "wal-crash-test-*")
	if err != nil {
		panic(err)
	}
	bin, kv = filepath.Join(dir, "wal-crash"), filepath.Join(dir, "kvstore")
	for out, pkg := range map[string]string{bin: ".", kv: "../../examples/kvstore"} {
		if b, err := exec.Command("go", "build", "-o", out, pkg).CombinedOutput(); err != nil {
			panic(string(b))
		}
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func needStrace(t *testing.T) {
	if _, err := exec.LookPath("strace"); err != nil {
		t.Skip("strace not installed")
	}
}

func run(t *testing.T, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	code := 0
	if e, ok := err.(*exec.ExitError); ok {
		code = e.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return out.String(), code
}

func TestRunCorrectEnginePasses(t *testing.T) {
	needStrace(t)
	out, code := run(t, "run", "--model", "all", "--checker", kv+" -engine log -dir . check",
		"--", kv, "-engine", "log", "-dir", "{dir}", "load", "4")
	if code != 0 || strings.Count(out, "PASS") != 3 {
		t.Fatalf("exit %d\n%s", code, out)
	}
}

func TestRunFindsAckBeforeFsync(t *testing.T) {
	needStrace(t)
	tracePath := filepath.Join(t.TempDir(), "t.json")
	out, code := run(t, "run", "-o", tracePath, "--checker", kv+" -engine log -bug ack-before-fsync -dir . check",
		"--", kv, "-engine", "log", "-bug", "ack-before-fsync", "-dir", "{dir}", "load", "4")
	if code != 1 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	for _, want := range []string{"FAIL", "LOST: file not fsynced after it", "acknowledged k1=v1 is missing", "wal-crash state " + tracePath} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q\n%s", want, out)
		}
	}

	// The reproduce line works: the state it writes out fails the checker
	// the same way when run by hand.
	id := strings.Fields(out[strings.Index(out, "wal-crash state "):])[3]
	dir := filepath.Join(t.TempDir(), "state")
	if out, code := run(t, "state", tracePath, id, "-o", dir); code != 0 {
		t.Fatalf("state: exit %d\n%s", code, out)
	}
	check := exec.Command(kv, "-engine", "log", "-dir", ".", "check")
	check.Dir = filepath.Join(dir, "state")
	check.Env = append(os.Environ(), "WAL_CRASH_ACKED="+filepath.Join(dir, "acked"), "WAL_CRASH_STDOUT="+filepath.Join(dir, "stdout"))
	b, err := check.CombinedOutput()
	if err == nil || !strings.Contains(string(b), "acknowledged k1=v1 is missing") {
		t.Errorf("reproduced state: %v %s", err, b)
	}
}

func TestRecordOpsCheck(t *testing.T) {
	needStrace(t)
	dir := t.TempDir()
	tracePath := filepath.Join(t.TempDir(), "t.json")
	out, code := run(t, "record", "-d", dir, "-o", tracePath, "--", kv, "-engine", "snapshot", "-dir", dir, "load", "2")
	if code != 0 || !strings.Contains(out, "recorded") {
		t.Fatalf("record: exit %d\n%s", code, out)
	}
	out, _ = run(t, "ops", tracePath)
	for _, want := range []string{"create   data.tmp", "rename   data.tmp -> data", "fsync    ./  (directory)", "stdout   ACK k2 v2"} {
		if !strings.Contains(out, want) {
			t.Errorf("ops lacks %q\n%s", want, out)
		}
	}
	out, code = run(t, "check", tracePath, "--checker", kv+" -engine snapshot -dir . check", "--json")
	if code != 0 || !strings.Contains(out, `"violations": null`) {
		t.Errorf("check: exit %d\n%s", code, out)
	}
}

func TestErrors(t *testing.T) {
	needStrace(t)
	if out, code := run(t, "run", "--checker", "true", "--", "sh", "-c", "exit 3"); code != 2 || !strings.Contains(out, "exited with status 3") {
		t.Errorf("failing workload: exit %d\n%s", code, out)
	}
	if out, code := run(t, "run", "--checker", "no-such-checker", "--", "true"); code != 2 || !strings.Contains(out, "checker did not run") {
		t.Errorf("broken checker: exit %d\n%s", code, out)
	}
	if out, code := run(t, "frobnicate"); code != 2 || !strings.Contains(out, "unknown command") {
		t.Errorf("unknown command: exit %d\n%s", code, out)
	}
	if out, code := run(t, "state", "missing.json", "c1", "-o", t.TempDir()); code != 2 {
		t.Errorf("missing trace: exit %d\n%s", code, out)
	}
}
