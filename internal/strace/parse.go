// Package strace records a workload with strace(1) and turns the system
// calls it made on one directory into a trace.
package strace

import (
	"bufio"
	"fmt"
	"io"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/ahmed-hashim-pro/wal-crash/internal/fsmodel"
	"github.com/ahmed-hashim-pro/wal-crash/internal/trace"
)

// desc is an open file description. dup'd descriptors and forked children
// share one, as the kernel does, so they share the file position.
type desc struct {
	tracked bool // under the traced root
	stdout  bool // the workload's original standard output
	dir     bool
	ino     int
	pos     int64
	append  bool
	path    string // absolute, as last known
}

type proc struct {
	fds map[int]*desc
	cwd *string
}

type clone struct {
	parent      int
	files, fsys bool
}

type parser struct {
	root    string // absolute, canonical
	tr      *trace.Trace
	fs      *fsmodel.FS
	procs   map[int]*proc
	clones  map[int]clone
	first   int
	cwd     string
	out     []byte // stdout bytes not yet ended by a newline
	outSrc  int
	mmapped map[int]bool
	line    int
}

// Parse turns strace output into the trace's operations. root must be
// absolute and free of symlinks; cwd is the workload's starting directory.
func Parse(r io.Reader, tr *trace.Trace, root, cwd string) error {
	return parseInto(r, tr, root, cwd, nil)
}

func parseInto(r io.Reader, tr *trace.Trace, root, cwd string, model **fsmodel.FS) error {
	fs, err := fsmodel.FromInitial(tr.Initial)
	if err != nil {
		return err
	}
	if model != nil {
		*model = fs
	}
	p := &parser{root: root, tr: tr, fs: fs, procs: map[int]*proc{}, clones: map[int]clone{},
		cwd: cwd, mmapped: map[int]bool{}}

	// First pass: every call, in the order the kernel finished them, and
	// which process each new pid was cloned from. A child can print its
	// first calls before strace prints its parent's clone returning.
	lp := &lineParser{pending: map[int]string{}}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 1<<30)
	var calls []*call
	n := 0
	for sc.Scan() {
		n++
		c, err := lp.parse(sc.Text(), n)
		if err != nil {
			return err
		}
		if c == nil {
			continue
		}
		if p.first == 0 {
			p.first = c.pid
		}
		switch c.name {
		case "clone", "clone3", "fork", "vfork":
			if child := c.retInt(); child > 0 {
				a := strings.Join(c.args, ",")
				p.clones[int(child)] = clone{parent: c.pid,
					files: strings.Contains(a, "CLONE_FILES"), fsys: strings.Contains(a, "CLONE_FS")}
			}
		}
		calls = append(calls, c)
	}
	if err := sc.Err(); err != nil {
		return err
	}
	for _, c := range calls {
		p.line = c.line
		if err := p.call(c); err != nil {
			return fmt.Errorf("strace line %d (%s): %w", c.line, c.name, err)
		}
	}
	p.flushOut(true)
	return nil
}

func (p *parser) proc(pid int) *proc {
	if pr, ok := p.procs[pid]; ok {
		return pr
	}
	var pr *proc
	if cl, ok := p.clones[pid]; ok {
		parent := p.proc(cl.parent)
		pr = &proc{fds: parent.fds, cwd: parent.cwd}
		if !cl.files {
			pr.fds = make(map[int]*desc, len(parent.fds))
			for k, v := range parent.fds {
				pr.fds[k] = v
			}
		}
		if !cl.fsys {
			c := *parent.cwd
			pr.cwd = &c
		}
	} else {
		c := p.cwd
		pr = &proc{cwd: &c, fds: map[int]*desc{0: {path: "stdin"}, 1: {stdout: true, path: "stdout"}, 2: {path: "stderr"}}}
	}
	p.procs[pid] = pr
	return pr
}

// rel returns the path relative to the root, if it is inside it.
func (p *parser) rel(abs string) (string, bool) {
	if abs == p.root {
		return ".", true
	}
	if strings.HasPrefix(abs, p.root+"/") {
		return abs[len(p.root)+1:], true
	}
	return "", false
}

// resolve makes a path argument absolute against a directory fd argument
// ("" for calls with no dirfd).
func (p *parser) resolve(pr *proc, dirfd, pathArg string) (string, error) {
	b, trunc, err := unquote(pathArg)
	if err != nil || trunc {
		return "", fmt.Errorf("bad path argument %.40q", pathArg)
	}
	s := string(b)
	if filepath.IsAbs(s) {
		return filepath.Clean(s), nil
	}
	base := *pr.cwd
	if dirfd != "" {
		fd, dp := fdArg(dirfd)
		switch {
		case dp != "":
			base = dp
		case fd == -100:
		default:
			d := pr.fds[fd]
			if d == nil {
				return "", fmt.Errorf("directory fd %d is not open", fd)
			}
			base = d.path
		}
	}
	return filepath.Clean(filepath.Join(base, s)), nil
}

func (p *parser) emit(o trace.Op) {
	o.Src = p.line
	p.fs.Apply(&o, fsmodel.Whole)
	p.tr.Ops = append(p.tr.Ops, o)
}

func (p *parser) warn(format string, a ...any) {
	p.tr.Warnings = append(p.tr.Warnings, fmt.Sprintf("strace line %d: ", p.line)+fmt.Sprintf(format, a...))
}

// pathOf returns a display path for an inode (best effort).
func (p *parser) pathOf(ino int) string {
	if ino == fsmodel.Root {
		return "."
	}
	found := ""
	p.fs.Walk(func(rel string, in *fsmodel.Inode) {
		if found == "" && p.fs.Inodes[ino] == in {
			found = rel
		}
	})
	if found == "" {
		return fmt.Sprintf("(inode %d)", ino)
	}
	return found
}

// parentOf splits a relative path into its directory inode and name.
func (p *parser) parentOf(rel string) (int, string, error) {
	dir, name := path.Split(rel)
	d, ok := p.fs.Lookup(strings.TrimSuffix(dir, "/"))
	if !ok || !p.fs.Inodes[d].Dir {
		return 0, "", fmt.Errorf("%s: parent directory is not in the recorded model", rel)
	}
	return d, name, nil
}

func (p *parser) fd(pr *proc, arg string) (int, *desc) {
	n, _ := fdArg(arg)
	return n, pr.fds[n]
}

func (p *parser) call(c *call) error {
	pr := p.proc(c.pid)
	ret := c.retInt()
	switch c.name {
	case "clone", "clone3", "fork", "vfork":
		if ret > 0 {
			p.proc(int(ret))
		}
	case "open", "openat", "openat2", "creat":
		if ret < 0 {
			return nil
		}
		return p.open(pr, c, int(ret))
	case "close":
		if ret == 0 {
			n, _ := fdArg(c.arg(0))
			delete(pr.fds, n)
		}
	case "close_range":
		if ret == 0 && !strings.Contains(c.arg(2), "CLOSE_RANGE_CLOEXEC") {
			lo, _ := strconv.ParseUint(c.arg(0), 0, 64)
			hi, _ := strconv.ParseUint(c.arg(1), 0, 64)
			for fd := range pr.fds {
				if uint64(fd) >= lo && uint64(fd) <= hi {
					delete(pr.fds, fd)
				}
			}
		}
	case "dup", "dup2", "dup3":
		if ret >= 0 {
			if _, d := p.fd(pr, c.arg(0)); d != nil {
				pr.fds[int(ret)] = d
			}
		}
	case "fcntl", "fcntl64":
		if ret >= 0 && strings.HasPrefix(c.arg(1), "F_DUPFD") {
			if _, d := p.fd(pr, c.arg(0)); d != nil {
				pr.fds[int(ret)] = d
			}
		}
	case "chdir":
		if ret == 0 {
			abs, err := p.resolve(pr, "", c.arg(0))
			if err != nil {
				return err
			}
			*pr.cwd = abs
		}
	case "fchdir":
		if ret == 0 {
			if _, d := p.fd(pr, c.arg(0)); d != nil {
				*pr.cwd = d.path
			}
		}
	case "read", "readv":
		if _, d := p.fd(pr, c.arg(0)); d != nil && ret > 0 {
			d.pos += ret
		}
	case "lseek":
		if _, d := p.fd(pr, c.arg(0)); d != nil && ret >= 0 {
			d.pos = ret
		}
	case "write", "writev", "pwrite64", "pwritev", "pwritev2":
		return p.write(pr, c, ret)
	case "ftruncate", "truncate":
		if ret != 0 {
			return nil
		}
		var ino int
		if c.name == "ftruncate" {
			_, d := p.fd(pr, c.arg(0))
			if d == nil || !d.tracked {
				return nil
			}
			ino = d.ino
		} else {
			abs, err := p.resolve(pr, "", c.arg(0))
			if err != nil {
				return err
			}
			rel, in := p.rel(abs)
			if !in {
				return nil
			}
			var ok bool
			if ino, ok = p.fs.Lookup(rel); !ok {
				return fmt.Errorf("truncate of %s, which the model doesn't have", rel)
			}
		}
		size, err := strconv.ParseInt(c.arg(1), 0, 64)
		if err != nil {
			return err
		}
		p.emit(trace.Op{Kind: trace.Truncate, Ino: ino, Size: size, Path: p.pathOf(ino)})
	case "fallocate":
		_, d := p.fd(pr, c.arg(0))
		if ret != 0 || d == nil || !d.tracked {
			return nil
		}
		off, _ := strconv.ParseInt(c.arg(2), 0, 64)
		n, _ := strconv.ParseInt(c.arg(3), 0, 64)
		switch mode := c.arg(1); {
		case mode == "0":
			if end := off + n; end > int64(len(p.fs.Inodes[d.ino].Data)) {
				p.emit(trace.Op{Kind: trace.Truncate, Ino: d.ino, Size: end, Path: p.pathOf(d.ino)})
			}
		case strings.Contains(mode, "FALLOC_FL_KEEP_SIZE") && !strings.Contains(mode, "PUNCH"):
		default:
			return fmt.Errorf("fallocate mode %s is not supported", mode)
		}
	case "fsync", "fdatasync":
		_, d := p.fd(pr, c.arg(0))
		if ret != 0 || d == nil || !d.tracked {
			return nil
		}
		if d.dir {
			p.emit(trace.Op{Kind: trace.FsyncDir, Ino: d.ino, Path: p.pathOf(d.ino)})
		} else {
			p.emit(trace.Op{Kind: trace.Fsync, Ino: d.ino, Path: p.pathOf(d.ino)})
		}
	case "sync", "syncfs":
		if ret == 0 {
			p.emit(trace.Op{Kind: trace.Sync})
		}
	case "rename", "renameat", "renameat2":
		if ret != 0 {
			return nil
		}
		return p.rename(pr, c)
	case "unlink", "unlinkat", "rmdir":
		if ret != 0 {
			return nil
		}
		return p.unlink(pr, c)
	case "mkdir", "mkdirat":
		if ret != 0 {
			return nil
		}
		dirfd, pa := "", c.arg(0)
		if c.name == "mkdirat" {
			dirfd, pa = c.arg(0), c.arg(1)
		}
		abs, err := p.resolve(pr, dirfd, pa)
		if err != nil {
			return err
		}
		rel, in := p.rel(abs)
		if !in {
			return nil
		}
		dir, name, err := p.parentOf(rel)
		if err != nil {
			return err
		}
		p.emit(trace.Op{Kind: trace.Mkdir, Dir: dir, Name: name, Ino: p.fs.Next, Path: rel})
	case "link", "linkat", "symlink", "symlinkat":
		if ret != 0 {
			return nil
		}
		for _, a := range c.args {
			if strings.HasPrefix(a, `"`) {
				b, _, _ := unquote(a)
				abs := string(b)
				if !filepath.IsAbs(abs) {
					abs = filepath.Join(*pr.cwd, abs)
				}
				if _, in := p.rel(filepath.Clean(abs)); in {
					return fmt.Errorf("hard and symbolic links are not supported")
				}
			}
		}
	case "mmap", "mmap2":
		_, d := p.fd(pr, c.arg(4))
		if d != nil && d.tracked && strings.Contains(c.arg(2), "PROT_WRITE") && strings.Contains(c.arg(3), "MAP_SHARED") && !p.mmapped[d.ino] {
			p.mmapped[d.ino] = true
			p.warn("%s is mapped shared and writable: writes through the mapping are not recorded, so crash states treat them as never reaching the disk", p.pathOf(d.ino))
		}
	case "copy_file_range", "sendfile", "sendfile64", "splice":
		for _, i := range []int{0, 1, 2} {
			if _, d := p.fd(pr, c.arg(i)); d != nil && d.tracked && ret > 0 {
				return fmt.Errorf("%s on a traced file is not supported", c.name)
			}
		}
	case "io_uring_setup":
		p.warn("the workload set up io_uring; I/O submitted through it is not recorded")
	}
	return nil
}

func (p *parser) open(pr *proc, c *call, fd int) error {
	var dirfd, pa, fl string
	switch c.name {
	case "open":
		pa, fl = c.arg(0), c.arg(1)
	case "creat":
		pa, fl = c.arg(0), "O_CREAT|O_WRONLY|O_TRUNC"
	case "openat":
		dirfd, pa, fl = c.arg(0), c.arg(1), c.arg(2)
	case "openat2":
		dirfd, pa = c.arg(0), c.arg(1)
		a := c.arg(2)
		if i := strings.Index(a, "flags="); i >= 0 {
			fl = strings.SplitN(a[i+6:], ",", 2)[0]
		}
	}
	abs := c.retPath()
	if abs == "" {
		var err error
		if abs, err = p.resolve(pr, dirfd, pa); err != nil {
			return err
		}
	}
	rel, in := p.rel(abs)
	if !in {
		pr.fds[fd] = &desc{path: abs}
		return nil
	}
	f := flags(fl)
	if f["O_TMPFILE"] || f["__O_TMPFILE"] {
		return fmt.Errorf("O_TMPFILE is not supported")
	}
	ino, exists := p.fs.Lookup(rel)
	switch {
	case !exists && f["O_CREAT"]:
		dir, name, err := p.parentOf(rel)
		if err != nil {
			return err
		}
		ino = p.fs.Next
		p.emit(trace.Op{Kind: trace.Create, Dir: dir, Name: name, Ino: ino, Path: rel})
	case !exists:
		return fmt.Errorf("opened %s, which the model doesn't have", rel)
	case f["O_TRUNC"] && !p.fs.Inodes[ino].Dir && len(p.fs.Inodes[ino].Data) > 0:
		p.emit(trace.Op{Kind: trace.Truncate, Ino: ino, Size: 0, Path: rel})
	}
	pr.fds[fd] = &desc{tracked: true, dir: p.fs.Inodes[ino].Dir, ino: ino, append: f["O_APPEND"], path: abs}
	return nil
}

func (p *parser) write(pr *proc, c *call, ret int64) error {
	n, d := p.fd(pr, c.arg(0))
	if ret <= 0 {
		return nil
	}
	if d == nil {
		if _, dp := fdArg(c.arg(0)); dp != "" {
			if _, in := p.rel(dp); in {
				return fmt.Errorf("write to fd %d (%s), which was never seen being opened", n, dp)
			}
		}
		return nil
	}
	if !d.tracked && !d.stdout {
		return nil
	}
	var data []byte
	switch c.name {
	case "write", "pwrite64":
		b, trunc, err := unquote(c.arg(1))
		if err != nil {
			return err
		}
		if trunc && int64(len(b)) < ret {
			return fmt.Errorf("strace truncated the written data (raise -s)")
		}
		data = b
	default: // writev, pwritev, pwritev2: concatenate iov_base strings
		a := c.arg(1)
		for {
			i := strings.Index(a, `iov_base="`)
			if i < 0 {
				break
			}
			a = a[i+len("iov_base="):]
			j := 1
			for j < len(a) && a[j] != '"' {
				if a[j] == '\\' {
					j++
				}
				j++
			}
			end := j + 1
			if strings.HasPrefix(a[end:], "...") {
				end += 3
			}
			b, _, err := unquote(a[:end])
			if err != nil {
				return err
			}
			data = append(data, b...)
			a = a[end:]
		}
		if int64(len(data)) < ret {
			return fmt.Errorf("strace truncated the written data (raise -s)")
		}
	}
	data = data[:ret]
	if d.stdout {
		p.stdout(data)
		return nil
	}
	if d.dir {
		return fmt.Errorf("write to a directory")
	}
	var off int64
	switch c.name {
	case "pwrite64":
		off, _ = strconv.ParseInt(c.arg(3), 0, 64)
	case "pwritev", "pwritev2":
		off, _ = strconv.ParseInt(c.arg(3), 0, 64)
	default:
		off = d.pos
		if d.append {
			off = int64(len(p.fs.Inodes[d.ino].Data))
		}
		d.pos = off + ret
	}
	p.emit(trace.Op{Kind: trace.Write, Ino: d.ino, Off: off, Data: append([]byte(nil), data...), Path: p.pathOf(d.ino)})
	return nil
}

func (p *parser) stdout(b []byte) {
	if len(p.out) == 0 {
		p.outSrc = p.line
	}
	p.out = append(p.out, b...)
	p.flushOut(false)
}

func (p *parser) flushOut(final bool) {
	for {
		i := strings.IndexByte(string(p.out), '\n')
		if i < 0 {
			if final && len(p.out) > 0 {
				i = len(p.out)
			} else {
				return
			}
		}
		line := string(p.out[:i])
		if i < len(p.out) {
			p.out = p.out[i+1:]
		} else {
			p.out = nil
		}
		o := trace.Op{Kind: trace.Out, Line: line, Ack: p.tr.AckPrefix != "" && strings.HasPrefix(line, p.tr.AckPrefix), Src: p.line}
		p.tr.Ops = append(p.tr.Ops, o)
	}
}

func (p *parser) rename(pr *proc, c *call) error {
	var od, op, nd, np string
	if c.name == "rename" {
		op, np = c.arg(0), c.arg(1)
	} else {
		od, op, nd, np = c.arg(0), c.arg(1), c.arg(2), c.arg(3)
		if c.name == "renameat2" && strings.Contains(c.arg(4), "RENAME_EXCHANGE") {
			return fmt.Errorf("RENAME_EXCHANGE is not supported")
		}
	}
	from, err := p.resolve(pr, od, op)
	if err != nil {
		return err
	}
	to, err := p.resolve(pr, nd, np)
	if err != nil {
		return err
	}
	fr, fin := p.rel(from)
	tr, tin := p.rel(to)
	switch {
	case !fin && !tin:
		return nil
	case fin != tin:
		return fmt.Errorf("rename across the edge of the traced directory (%s -> %s)", from, to)
	}
	ino, ok := p.fs.Lookup(fr)
	if !ok {
		return fmt.Errorf("rename of %s, which the model doesn't have", fr)
	}
	d1, n1, err := p.parentOf(fr)
	if err != nil {
		return err
	}
	d2, n2, err := p.parentOf(tr)
	if err != nil {
		return err
	}
	p.emit(trace.Op{Kind: trace.Rename, Dir: d1, Name: n1, Dir2: d2, Name2: n2, Ino: ino, Path: fr, To: tr})
	return nil
}

func (p *parser) unlink(pr *proc, c *call) error {
	dirfd, pa, kind := "", c.arg(0), trace.Unlink
	switch c.name {
	case "rmdir":
		kind = trace.Rmdir
	case "unlinkat":
		dirfd, pa = c.arg(0), c.arg(1)
		if strings.Contains(c.arg(2), "AT_REMOVEDIR") {
			kind = trace.Rmdir
		}
	}
	abs, err := p.resolve(pr, dirfd, pa)
	if err != nil {
		return err
	}
	rel, in := p.rel(abs)
	if !in {
		return nil
	}
	ino, ok := p.fs.Lookup(rel)
	if !ok {
		return fmt.Errorf("unlink of %s, which the model doesn't have", rel)
	}
	dir, name, err := p.parentOf(rel)
	if err != nil {
		return err
	}
	p.emit(trace.Op{Kind: kind, Dir: dir, Name: name, Ino: ino, Path: rel})
	return nil
}
