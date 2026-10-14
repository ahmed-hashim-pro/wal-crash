// Command matrix runs every example under every persistence model and
// prints the tables in the README. With -check it also fails if any verdict
// differs from what the README claims, which is how CI keeps them honest.
//
//	go run ./examples/matrix -bin bin [-check] [-sqlite]
package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/ahmed-hashim-pro/wal-crash/internal/check"
	"github.com/ahmed-hashim-pro/wal-crash/internal/crash"
	"github.com/ahmed-hashim-pro/wal-crash/internal/strace"
)

type row struct {
	label   string
	argv    []string // {dir} is the data directory
	checker string
	want    string // expected verdicts for process, ordered, posix: "ppF"
}

func main() {
	bin := flag.String("bin", "bin", "directory holding wal-crash's example binaries")
	checkClaims := flag.Bool("check", false, "fail if a verdict differs from the README's")
	sqlite := flag.Bool("sqlite", true, "include the SQLite table")
	flag.Parse()
	b, err := filepath.Abs(*bin)
	if err != nil {
		die(err)
	}
	kv := filepath.Join(b, "kvstore")

	var kvRows []row
	for _, c := range []struct{ engine, bug, want string }{
		{"snapshot", "", "ppp"},
		{"snapshot", "write-in-place", "FFF"},
		{"snapshot", "skip-file-fsync", "ppF"},
		{"snapshot", "skip-dir-fsync", "ppF"},
		{"log", "", "ppp"},
		{"log", "ack-before-fsync", "ppF"},
		{"log", "skip-create-dir-fsync", "ppF"},
		{"log", "no-crc", "pFF"},
	} {
		label := c.bug
		if label == "" {
			label = "correct"
		}
		args := fmt.Sprintf("-engine %s -bug '%s'", c.engine, c.bug)
		kvRows = append(kvRows, row{
			label:   fmt.Sprintf("%-9s %s", c.engine, label),
			argv:    []string{"sh", "-c", fmt.Sprintf("%s %s -dir {dir} load 5", kv, args)},
			checker: fmt.Sprintf("%s %s -dir . check", kv, args),
			want:    c.want,
		})
	}
	bad := table("kvstore, 5 puts", kvRows, *checkClaims)

	if *sqlite {
		version, _ := exec.Command("sqlite3", "--version").Output()
		v := strings.Fields(string(version) + " ?")[0]
		check := filepath.Join(filepath.Dir(b), "examples", "sqlite", "check.sh")
		var rows []row
		for _, c := range []struct{ mode, sync, want string }{
			{"delete", "off", "ppF"},
			{"delete", "normal", "ppF"},
			{"delete", "full", "ppF"},
			{"delete", "extra", "ppp"},
			{"wal", "off", "ppF"},
			{"wal", "normal", "ppF"},
			{"wal", "full", "ppp"},
			{"wal", "extra", "ppp"},
		} {
			rows = append(rows, row{
				label:   fmt.Sprintf("journal_mode=%-6s synchronous=%s", c.mode, c.sync),
				argv:    []string{filepath.Join(b, "sqlite-workload"), "{dir}/test.db", c.mode, c.sync, "6"},
				checker: check,
				want:    c.want,
			})
		}
		fmt.Println()
		bad += table("SQLite "+v+", 6 transactions", rows, *checkClaims)
	}
	if bad > 0 {
		fmt.Fprintf(os.Stderr, "\nmatrix: %d verdicts differ from the README\n", bad)
		os.Exit(1)
	}
}

func table(title string, rows []row, claims bool) int {
	fmt.Printf("%-40s %-8s %-8s %-8s %s\n", title, "process", "ordered", "posix", "simplest failure, weakest model that finds it")
	bad := 0
	for _, r := range rows {
		got, why, err := verdicts(r)
		if err != nil {
			die(fmt.Errorf("%s: %w", r.label, err))
		}
		cols := make([]string, 3)
		for i, c := range got {
			cols[i] = map[byte]string{'p': "pass", 'F': "FAIL"}[byte(c)]
		}
		mark := ""
		if claims && got != r.want {
			mark = fmt.Sprintf("   <-- README says %s", r.want)
			bad++
		}
		fmt.Printf("%-40s %-8s %-8s %-8s %s%s\n", r.label, cols[0], cols[1], cols[2], why, mark)
	}
	return bad
}

// verdicts records the row's workload once and checks it under each model.
func verdicts(r row) (string, string, error) {
	dir, err := os.MkdirTemp("", "matrix-*")
	if err != nil {
		return "", "", err
	}
	defer os.RemoveAll(dir)
	argv := make([]string, len(r.argv))
	for i, a := range r.argv {
		argv[i] = strings.ReplaceAll(a, "{dir}", dir)
	}
	tr, err := strace.Record(argv, strace.Options{Root: dir, AckPrefix: "ACK "})
	if err != nil {
		return "", "", err
	}
	e, err := crash.New(tr)
	if err != nil {
		return "", "", err
	}
	out, why := "", ""
	for _, m := range crash.Models {
		rep, err := check.Run(e, m, check.Options{Checker: r.checker})
		if err != nil {
			return "", "", err
		}
		if len(rep.Violations) == 0 {
			out += "p"
			continue
		}
		out += "F"
		if why == "" {
			why, _, _ = strings.Cut(rep.Violations[0].Output, "\n")
			why = strings.TrimPrefix(why, "Error: ")
			if len(why) > 60 {
				why = why[:59] + "…"
			}
		}
	}
	return out, why, nil
}

func die(err error) {
	fmt.Fprintln(os.Stderr, "matrix:", err)
	os.Exit(2)
}
