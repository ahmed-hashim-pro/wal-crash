package strace

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/ahmed-hashim-pro/wal-crash/internal/fsmodel"
	"github.com/ahmed-hashim-pro/wal-crash/internal/trace"
)

var update = flag.Bool("update", false, "rewrite the golden .ops files")

// The .strace files are real strace output, recorded with Args() in the
// dev image with /tmp/golden as the traced directory:
//
//	kvstore -engine snapshot -dir /tmp/golden load 3
//	sqlite-workload /tmp/golden/test.db wal full 2
//
// so the parser is tested against what the kernel and Go's runtime and
// SQLite actually do, even where strace is not installed.
func TestGolden(t *testing.T) {
	for _, name := range []string{"kvstore-snapshot", "sqlite-wal"} {
		t.Run(name, func(t *testing.T) {
			f, err := os.Open("testdata/" + name + ".strace")
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			tr := &trace.Trace{Version: trace.Version, AckPrefix: "ACK "}
			var model *fsmodel.FS
			if err := parseInto(f, tr, "/tmp/golden", "/w", &model); err != nil {
				t.Fatal(err)
			}
			var b strings.Builder
			for i := range tr.Ops {
				fmt.Fprintf(&b, "%4d  %s\n", i+1, tr.Ops[i].String())
			}
			for _, w := range tr.Warnings {
				fmt.Fprintf(&b, "warning: %s\n", w)
			}
			b.WriteString("final:\n")
			model.Walk(func(p string, in *fsmodel.Inode) {
				fmt.Fprintf(&b, "  %s %d B\n", p, len(in.Data))
			})
			golden := "testdata/" + name + ".ops"
			if *update {
				os.WriteFile(golden, []byte(b.String()), 0o644)
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatal(err)
			}
			if b.String() != string(want) {
				t.Errorf("ops differ from %s (go test -update to rewrite):\n%s", golden, b.String())
			}
		})
	}
}
