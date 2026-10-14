// Command kvstore is a deliberately small key-value store with two storage
// engines and six planted durability bugs, to show what wal-crash finds.
//
//	kvstore -engine snapshot|log [-bug NAME] -dir DIR load N
//	kvstore -engine snapshot|log [-bug NAME] -dir DIR check
//	kvstore bugs
//
// load puts keys k1..kN, printing "PUT k v" before each put and "ACK k v"
// once the store has promised it is durable. check is the recovery path
// plus the invariant, written for wal-crash: it opens the store the way a
// restart would and fails if an acknowledged put is missing or if any
// value is present that was never written.
package main

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"hash/crc32"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

var bugs = []struct{ engine, name, what string }{
	{"snapshot", "write-in-place", "overwrites the data file directly instead of writing a new one and renaming it"},
	{"snapshot", "skip-file-fsync", "renames the new file into place without fsyncing it first"},
	{"snapshot", "skip-dir-fsync", "never fsyncs the directory after the rename"},
	{"log", "ack-before-fsync", "acknowledges a put before fsyncing the log"},
	{"log", "skip-create-dir-fsync", "creates the log file but never fsyncs the directory"},
	{"log", "no-crc", "recovery trusts a record's length and ignores its checksum"},
}

func main() {
	engine := flag.String("engine", "log", "snapshot or log")
	bug := flag.String("bug", "", "planted bug to switch on (see: kvstore bugs)")
	dir := flag.String("dir", ".", "data directory")
	flag.Parse()
	if flag.Arg(0) == "bugs" {
		for _, b := range bugs {
			fmt.Printf("%-9s %-22s %s\n", b.engine, b.name, b.what)
		}
		return
	}
	if *bug != "" {
		ok := false
		for _, b := range bugs {
			ok = ok || (b.engine == *engine && b.name == *bug)
		}
		if !ok {
			die(fmt.Errorf("engine %s has no bug %q (see: kvstore bugs)", *engine, *bug))
		}
	}
	var s store
	switch *engine {
	case "snapshot":
		s = &snapshot{dir: *dir, bug: *bug}
	case "log":
		s = &logStore{dir: *dir, bug: *bug}
	default:
		die(fmt.Errorf("unknown engine %q", *engine))
	}
	switch flag.Arg(0) {
	case "load":
		n, err := strconv.Atoi(flag.Arg(1))
		if err != nil {
			die(errors.New("usage: kvstore load N"))
		}
		die(load(s, n))
	case "check":
		if err := check(s); err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
	default:
		die(errors.New("usage: kvstore [-engine E] [-bug B] -dir DIR load N | check | bugs"))
	}
}

func die(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "kvstore:", err)
		os.Exit(2)
	}
}

type store interface {
	open() error
	put(k, v string, ack func()) error
	recover() (map[string]string, error)
}

func load(s store, n int) error {
	if err := s.open(); err != nil {
		return err
	}
	for i := 1; i <= n; i++ {
		k, v := fmt.Sprintf("k%d", i), fmt.Sprintf("v%d", i)
		fmt.Printf("PUT %s %s\n", k, v)
		acked := false
		ack := func() { fmt.Printf("ACK %s %s\n", k, v); acked = true }
		if err := s.put(k, v, ack); err != nil {
			return err
		}
		if !acked {
			ack()
		}
	}
	return nil
}

// check is the invariant: recovery succeeds, every acknowledged put is
// there with its value, and nothing is there that was never put.
func check(s store) error {
	m, err := s.recover()
	if err != nil {
		return fmt.Errorf("recovery failed: %v", err)
	}
	acked, err := lines(os.Getenv("WAL_CRASH_ACKED"))
	if err != nil {
		return err
	}
	out, err := lines(os.Getenv("WAL_CRASH_STDOUT"))
	if err != nil {
		return err
	}
	written := map[string]bool{}
	for _, l := range out {
		if rest, ok := strings.CutPrefix(l, "PUT "); ok {
			written[rest] = true
		}
	}
	for _, a := range acked {
		k, v, _ := strings.Cut(a, " ")
		if got, ok := m[k]; !ok {
			return fmt.Errorf("acknowledged %s=%s is missing", k, v)
		} else if got != v {
			return fmt.Errorf("acknowledged %s=%s reads back as %q", k, v, got)
		}
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !written[k+" "+m[k]] {
			return fmt.Errorf("%q=%q was never written", k, m[k])
		}
	}
	return nil
}

func lines(path string) ([]string, error) {
	if path == "" {
		return nil, errors.New("run me from wal-crash: $WAL_CRASH_ACKED is not set")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []string
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		out = append(out, sc.Text())
	}
	return out, nil
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// snapshot keeps the whole map in one file, replaced on every put by the
// classic write-temp, fsync, rename, fsync-directory sequence.
type snapshot struct {
	dir, bug string
	m        map[string]string
}

func (s *snapshot) open() error {
	m, err := s.recover()
	s.m = m
	return err
}

func (s *snapshot) put(k, v string, ack func()) error {
	s.m[k] = v
	data := encodeSnapshot(s.m)
	final := filepath.Join(s.dir, "data")
	if s.bug == "write-in-place" {
		f, err := os.OpenFile(final, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
		if err != nil {
			return err
		}
		if _, err := f.Write(data); err != nil {
			return err
		}
		if err := f.Sync(); err != nil {
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
		return syncDir(s.dir)
	}
	tmp := final + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		return err
	}
	if s.bug != "skip-file-fsync" {
		if err := f.Sync(); err != nil {
			return err
		}
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, final); err != nil {
		return err
	}
	if s.bug != "skip-dir-fsync" {
		return syncDir(s.dir)
	}
	return nil
}

// encodeSnapshot: "k=v" lines in key order, then "crc <crc32 of the lines>".
func encodeSnapshot(m map[string]string) []byte {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b bytes.Buffer
	for _, k := range keys {
		fmt.Fprintf(&b, "%s=%s\n", k, m[k])
	}
	fmt.Fprintf(&b, "crc %08x\n", crc32.ChecksumIEEE(b.Bytes()))
	return b.Bytes()
}

func (s *snapshot) recover() (map[string]string, error) {
	m := map[string]string{}
	b, err := os.ReadFile(filepath.Join(s.dir, "data"))
	if errors.Is(err, fs.ErrNotExist) {
		return m, nil
	}
	if err != nil {
		return nil, err
	}
	i := bytes.LastIndex(b, []byte("crc "))
	if i < 0 || fmt.Sprintf("crc %08x\n", crc32.ChecksumIEEE(b[:i])) != string(b[i:]) {
		return nil, fmt.Errorf("data file is corrupt (%d bytes, checksum does not match)", len(b))
	}
	for _, l := range strings.Split(strings.TrimSuffix(string(b[:i]), "\n"), "\n") {
		if l == "" {
			continue
		}
		k, v, _ := strings.Cut(l, "=")
		m[k] = v
	}
	return m, nil
}

// logStore appends one record per put to a log file: 4-byte length, 4-byte
// CRC-32 of the payload, payload "k=v". Recovery replays records up to the
// first incomplete or damaged one: that is the torn tail of a crash.
type logStore struct {
	dir, bug string
	f        *os.File
}

func (s *logStore) open() error {
	path := filepath.Join(s.dir, "log")
	_, err := os.Stat(path)
	fresh := errors.Is(err, fs.ErrNotExist)
	s.f, err = os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	if fresh && s.bug != "skip-create-dir-fsync" {
		return syncDir(s.dir)
	}
	return nil
}

func (s *logStore) put(k, v string, ack func()) error {
	payload := []byte(k + "=" + v)
	rec := make([]byte, 8+len(payload))
	binary.LittleEndian.PutUint32(rec[0:], uint32(len(payload)))
	binary.LittleEndian.PutUint32(rec[4:], crc32.ChecksumIEEE(payload))
	copy(rec[8:], payload)
	if _, err := s.f.Write(rec); err != nil {
		return err
	}
	if s.bug == "ack-before-fsync" {
		ack()
	}
	return s.f.Sync()
}

func (s *logStore) recover() (map[string]string, error) {
	m := map[string]string{}
	b, err := os.ReadFile(filepath.Join(s.dir, "log"))
	if errors.Is(err, fs.ErrNotExist) {
		return m, nil
	}
	if err != nil {
		return nil, err
	}
	for len(b) >= 8 {
		n := binary.LittleEndian.Uint32(b[0:])
		sum := binary.LittleEndian.Uint32(b[4:])
		if n == 0 || uint64(len(b)-8) < uint64(n) {
			break // torn tail
		}
		payload := b[8 : 8+n]
		if s.bug != "no-crc" && crc32.ChecksumIEEE(payload) != sum {
			break // torn tail
		}
		k, v, _ := strings.Cut(string(payload), "=")
		m[k] = v
		b = b[8+n:]
	}
	return m, nil
}
