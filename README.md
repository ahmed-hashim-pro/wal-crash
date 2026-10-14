# wal-crash

[![CI](https://github.com/ahmed-hashim-pro/wal-crash/actions/workflows/ci.yml/badge.svg)](https://github.com/ahmed-hashim-pro/wal-crash/actions/workflows/ci.yml) [![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

A crash-consistency tester for storage code. It records every file-system
operation a program makes, builds every state a power failure could leave
the directory in, runs the program's recovery on each one, and tells you
which promise broke and which lost operation broke it.

Storage code makes a promise each time it says "committed": the data will
still be there after a crash. Testing that promise by killing the process
proves very little. The operating system survives a `kill -9`, so its page
cache does too, and every write the program made is still there. A power
failure is different. Whatever no `fsync` covered may be lost, kept, or
half-written, in any combination, so a write can be lost while a later
rename of the same file survives. The bugs live in that difference: a
missing `fsync` of a directory, an acknowledgement sent one line too early,
a checksum the recovery code never checks. They pass every test that
doesn't simulate the disk.

```
$ wal-crash run --model all --checker "$kv -engine log -bug ack-before-fsync -dir . check" \
    -- $kv -engine log -bug ack-before-fsync -dir {dir} load 5
22 operations recorded, 5 acknowledged

process  23 states (17 distinct) checked   PASS
ordered  38 states (32 distinct) checked   PASS
posix    64 states (52 distinct) checked   FAIL  20 of them break the checker

simplest failure under the posix model:

  crash after op 5 of 22 (state c5.synced), 1 operation lost:

        2  fsync    ./  (directory)
        3  stdout   PUT k1 v1
   ✗    4  write    log  13 B @ 0                     LOST: file not fsynced after it
        5  stdout   ACK k1 v1

  checker: acknowledged k1=v1 is missing
```

![Terminal recording: wal-crash finds a log store that acknowledges a put before fsyncing it, which passes under the process and ordered models and fails under posix with the lost write marked; then SQLite in rollback-journal mode with synchronous=FULL fails because the journal's unlink was never made durable by a directory fsync, and the same workload with synchronous=EXTRA passes all 2,181 states](docs/demo.gif)

## What it found

### Six planted bugs in a small key-value store

[`examples/kvstore`](examples/kvstore/main.go) has two engines. `snapshot`
rewrites one file with the write-temp, fsync, rename, fsync-directory
sequence. `log` appends checksummed records and fsyncs each one. Each engine
can switch on three classic bugs. The correct engines pass every state under
every model, and every bug is caught:

| engine, bug | process | ordered | posix | simplest failure |
| --- | --- | --- | --- | --- |
| snapshot, correct | pass | pass | pass | |
| snapshot, `write-in-place` | **FAIL** | **FAIL** | **FAIL** | data file is empty: crash between truncate and write |
| snapshot, `skip-file-fsync` | pass | pass | **FAIL** | the rename survived, the data didn't: an empty file |
| snapshot, `skip-dir-fsync` | pass | pass | **FAIL** | acknowledged k1 missing: the rename was lost |
| log, correct | pass | pass | pass | |
| log, `ack-before-fsync` | pass | pass | **FAIL** | acknowledged k1 missing |
| log, `skip-create-dir-fsync` | pass | pass | **FAIL** | acknowledged k1 missing: the log file itself was lost |
| log, `no-crc` | pass | **FAIL** | **FAIL** | a torn record of zeros read back as a key |

Killing the process (the `process` model) finds one bug in six. Five of
them need a model of the disk.

### SQLite, every journal mode and sync level

The same harness drives SQLite 3.46.1 through
[a small C program](examples/sqlite/workload.c): six transactions, each
acknowledged once `COMMIT` returns. [The checker](examples/sqlite/check.sh)
opens the crash state with the `sqlite3` CLI, which runs SQLite's own
recovery (hot-journal rollback or WAL replay). It then requires
`PRAGMA integrity_check` to say `ok`, every acknowledged row to be present,
and no row that was never written.

| journal_mode, synchronous | process | ordered | posix | simplest failure |
| --- | --- | --- | --- | --- |
| delete, off | pass | pass | **FAIL** | database disk image is malformed |
| delete, normal | pass | pass | **FAIL** | integrity_check: btree page 3 is all zeros |
| delete, full | pass | pass | **FAIL** | acknowledged row k1 missing |
| delete, extra | pass | pass | pass | |
| wal, off | pass | pass | **FAIL** | acknowledged row k1 missing |
| wal, normal | pass | pass | **FAIL** | acknowledged row k1 missing |
| wal, full | pass | pass | pass | |
| wal, extra | pass | pass | pass | |

Every row agrees with what SQLite documents for
[`PRAGMA synchronous`](https://www.sqlite.org/pragma.html#pragma_synchronous),
and wal-crash shows the mechanism behind each one:

- **`delete, full` loses the last commit; `extra` doesn't.** In
  rollback-journal mode, deleting the journal *is* the commit. With `FULL`,
  SQLite fsyncs the database and then unlinks the journal, but doesn't
  fsync the directory. If the unlink is lost, recovery finds a hot journal
  and rolls the acknowledged transaction back. That's the case SQLite's
  documentation gives as the reason `EXTRA` exists: it also syncs the
  directory after the unlink.
- **`delete, normal` can corrupt the database.** With `NORMAL`, SQLite
  writes the journal's pages and then its header (which says how many pages
  follow), with one fsync after both instead of one between them. In the
  failing state the header reached the disk and one journal page didn't. Its
  slot reads as zeros, and the page's checksum still passed: SQLite's journal
  checksum samples only one byte in 200, and on that page those bytes were
  zero anyway. Recovery "restored" a page of zeros over a btree page.
  (wal-crash wrote the state out; the decoded journal is in the
  [notes below](#the-delete-normal-corruption).)
- **`wal, normal` loses commits but never corrupts.** The WAL isn't synced
  at each commit, so acknowledged transactions can vanish, but every
  failure is a missing row, never a broken database. That matches the docs:
  WAL mode with `NORMAL` stays consistent but gives up durability.
- **`off` survives a killed program but not a power cut**, as documented:
  every `off` row passes `process` and fails `posix`.

[`docs/matrix.txt`](docs/matrix.txt) is the raw output.
`make matrix` reruns both tables (about a minute) and fails if any verdict
changes. CI runs it on every push.

## Quickstart

wal-crash needs Linux, Go 1.23+ and `strace`. The examples also need
`sqlite3`, `libsqlite3-dev` and a C compiler. The Makefile runs everything
in a Docker image that has all of them, so Docker alone is enough:

```bash
git clone https://github.com/ahmed-hashim-pro/wal-crash.git
cd wal-crash
make build          # bin/wal-crash, bin/kvstore, bin/sqlite-workload (in Docker)
make test           # unit, golden and end-to-end tests
make matrix         # the two tables above
```

To use the binaries directly, start a shell in the same image:

```bash
docker run --rm -it -u $(id -u):$(id -g) -e HOME=/tmp -v $PWD:/w -w /w wal-crash-dev bash
export PATH=$PWD/bin:$PATH kv=$PWD/bin/kvstore

# a correct store: passes under every model
wal-crash run --model all --checker "$kv -engine log -dir . check" -- $kv -engine log -dir {dir} load 5

# a planted bug: fails under posix, with the lost operation marked
wal-crash run --checker "$kv -engine snapshot -bug skip-dir-fsync -dir . check" -o trace.json \
    -- $kv -engine snapshot -bug skip-dir-fsync -dir {dir} load 5

# write that crash state out and look at it
wal-crash state trace.json c6.drop5 -o /tmp/state

# SQLite, rollback journal, synchronous=FULL
wal-crash run --checker $PWD/examples/sqlite/check.sh -- sqlite-workload {dir}/test.db delete full 6
```

With a Linux machine that already has Go, strace and the SQLite headers,
`make build test matrix LOCAL=1` skips Docker.

## Testing your own program

You need two things:

1. **A workload** that does real work in one directory and prints a line
   starting with `ACK ` each time it makes a durability promise: after
   `COMMIT` returns, after a put is acknowledged, after a file is
   "saved". Pass the directory with `-d DIR`, or let wal-crash make an
   empty one and put its path wherever `{dir}` appears in the command.
2. **A checker**: a shell command that wal-crash runs inside each crash
   state's directory. It should open the data the way your program does
   after a restart (so your recovery code runs) and exit non-zero, saying
   why on its first line, if something is wrong. It gets:

   | variable | contents |
   | --- | --- |
   | `$WAL_CRASH_ACKED` | a file with each acknowledgement printed before the crash, `ACK ` removed |
   | `$WAL_CRASH_STDOUT` | a file with every line printed before the crash (use it to know what was *attempted*) |
   | `$WAL_CRASH_DIR` | the state's directory (also the working directory) |
   | `$WAL_CRASH_STATE` | the state's name, e.g. `c41.drop39` |

The usual invariant is: recovery succeeds, every acknowledged write is
there, and nothing is there that was never written. Writes that were in
flight at the crash may go either way.

```
wal-crash run    [flags] -- COMMAND...   record COMMAND, then check every crash state
wal-crash record [flags] -- COMMAND...   only record, to a trace file
wal-crash check  [flags] TRACE           check a recorded trace
wal-crash ops    TRACE                   list the recorded operations
wal-crash state  TRACE ID -o DIR         write one crash state out to look at
```

`--model process|ordered|posix|all` picks the persistence model (default
`posix`), `-j` sets parallel checkers, `--timeout` sets a time limit per
check (a hung recovery counts as a failure), and `--json` prints the report
as JSON. Exit status: 0 if every state passed, 1 if any failed, 2 if
wal-crash couldn't do the job.

## How it works

### Recording

`wal-crash record` runs the workload under
`strace -f -y -xx -s 64M`, which logs every file system call with the full
bytes written and the path behind every file descriptor. The parser
replays that log against an in-memory model of the directory:

- **Descriptors and positions.** Open file descriptions are shared the way
  the kernel shares them: `dup` and `fork` share the position, threads
  share the table, `O_APPEND` writes go to the end. That's how a plain
  `write()` gets its offset.
- **Operations.** It turns system calls into operations on inodes: create,
  mkdir, rename, unlink, rmdir, write, truncate, plus the sync points
  (`fsync`/`fdatasync` of a file or a directory, `sync`, `syncfs`).
  Standard output lines become operations too, so each acknowledgement sits
  at its exact place in the sequence.
- **Self-check.** After the run, the model's final state is compared with
  the real directory, and any difference is reported as a warning.
- **Refusals.** Things it can't model are refused, not ignored: hard and
  symbolic links, `RENAME_EXCHANGE`, `O_TMPFILE`, `copy_file_range`,
  `sendfile`. Shared writable `mmap` of a traced file gets a warning, because
  writes through the mapping are invisible to strace (see below).

The trace is plain JSON. [`internal/strace/testdata`](internal/strace/testdata)
has two real strace logs, from the Go store and from SQLite, and the
operations each one produces, so the parser is tested against real kernel
output in CI.

### Persistence models

The file system is a set of inodes. A directory maps names to inode
numbers, and every namespace operation names the directory inode it changes.
So an operation still makes sense when one it depended on was lost: a write
to a file whose creation never reached the disk lands on an inode nothing
points to, which is what a real disk would hold.

A crash happens after operation *c*. What survives depends on the model:

- **`process`**: the program dies, the OS doesn't. Every operation before
  *c* is kept, in order.
- **`ordered`**: power fails, and the disk wrote everything in the order it
  was issued. As `process`, plus the last write may be torn.
- **`posix`**: power fails, and only what `fsync` promised survives. An
  operation is *forced* if a sync completed before the crash that covers it:
  - `fsync` of a file covers that file's earlier writes and truncates;
  - `fsync` of a directory covers earlier entry changes in that directory
    (a rename between two directories needs both synced);
  - `sync` covers everything.

  Every other operation before *c* is *pending*: it may have reached the
  disk or not, independently of the others.

A torn write comes in three shapes:
- **half**: the first half of the bytes written, the rest still old or zeros
  where the write extended the file;
- **short**: the first half written, and the file ends there;
- **zeros**: the file grew, but the new bytes read as zeros.

### Which states are explored

All subsets of the pending operations would be 2^k states per crash point,
so like ALICE and CrashMonkey, wal-crash runs a bounded search. For each
crash point under `posix` it tries:
- everything pending kept;
- nothing pending kept;
- each pending operation lost alone;
- each pending operation kept alone;
- each pending write torn in each of the three shapes, with the others kept.

States with the same visible files and the same output before the crash are
checked once. The SQLite rows explore 1,900 to 22,600 states, which come
down to 550 to 2,000 distinct ones. Checks run in parallel, one temporary
directory each.

The failure reported is the *simplest*: fewest operations lost or torn,
then the earliest crash, then a lost operation over a torn one. Usually it
is one missing operation right before an acknowledgement, which reads
directly as the bug.

### The delete-normal corruption

`wal-crash state` wrote out the first failing state of
`journal_mode=delete, synchronous=normal` (crash after op 24, op 16 lost).
These are its journal records decoded:

```
journal 12824 bytes; header: nRec 3, original size 3 pages
record 0  page 3  data all zeros: True   checksum ok: True    <- op 16, lost
record 1  page 2  data all zeros: False  checksum ok: True
record 2  page 1  data all zeros: False  checksum ok: True
```

Op 24 rewrote the header with `nRec 3` and survived; op 16, page 3's
copy, didn't. SQLite's journal checksum adds the page's bytes at offsets
3896, 3696, … 96 to a random nonce. Those bytes of the original page 3 were
zeros, so a page of zeros has the same checksum. The rollback "restores"
it, and `PRAGMA integrity_check` reports the broken btree page. With `FULL`,
SQLite syncs the pages before it writes the header, so a header can never
outrun its pages, and that row passes.

## Tests

`make test` runs gofmt, vet and 34 test functions under the race detector,
in about 10 s:

- **lexer and parser:** strace's quoting, interrupted calls joined back
  together, file positions through `dup`, `fork` and threads, `O_APPEND`,
  `writev`, renames between directories, and refusals;
- **golden traces:** real strace output from the Go store and from SQLite;
- **model:** the three torn-write shapes, tolerant renames and unlinks,
  hashes that ignore unreachable inodes, a materialize and diff round trip;
- **models and enumeration:** what each sync forces, renames between two
  directories, that `posix` finds a lost rename which `process` and
  `ordered` can't, state names, simplicity ordering;
- **runner:** finding the simplest violation, deduplication, a broken
  checker failing fast, timeouts counting as failures;
- **end to end:** the real binary records the Go store under strace, finds
  the planted bug, and the `reproduce:` line writes out a state that fails
  the checker the same way.

`make matrix` is the last test: it reruns both tables and fails if any
verdict differs from the ones above.

## What this does not do

- **It is not exhaustive.** The search above is bounded. A bug that needs
  two particular pending operations lost and a third kept can be missed. In
  practice the bounded search found every bug here with one lost operation,
  but it can't promise more than it explores.
- **The models are not a specific file system.** `posix` is weaker than
  ext4, XFS or btrfs as configured by default: ext4 with `data=ordered`, for
  example, usually persists a file's data before a rename that replaces it.
  `ordered` is stronger than any of them. A program that passes `posix` is
  safe on a POSIX-conforming file system. A failure under `posix` is a real
  violation of the POSIX contract, but it may not happen on your file system
  as it is configured today.
- **Writes through shared `mmap`, and `io_uring`, are not recorded.** strace
  sees system calls, and a store through a mapping isn't one. Crash states
  treat such writes as never reaching the disk, which is a legal outcome but
  not the only one. SQLite's `-shm` file is the case here: it is rebuilt on
  recovery, so that is safe, and wal-crash says so in a warning.
- **One directory tree, regular files and directories only.** No links,
  permissions, extended attributes or special files. The program may run
  several processes and threads, but syscalls are ordered by when they
  finished, and two threads writing the same file concurrently are not
  modeled beyond that order.
- **It doesn't judge correctness.** The checker does. A weak checker
  (one that only opens the database, say) will pass states a strong one
  would fail.
- **Recovery runs in a different directory.** Each state is written to a
  fresh temporary directory, so a program that stored absolute paths in its
  data, and checks them on recovery, needs its checker to account for that.
- **It is slow on big workloads.** strace slows the workload down, and the
  number of states grows with the number of operations times the number
  pending. It is meant for small, focused workloads (six SQLite
  transactions, not a benchmark).
- **Linux only.** It needs `strace`, which needs `ptrace`. Docker's default
  seccomp profile allows it; some locked-down CI runners and containers
  don't.

## Layout

```
cmd/wal-crash/       the command line
internal/strace/     recording: strace arguments, output parser, descriptor tracking
internal/fsmodel/    the inode file system crash states are built in
internal/crash/      persistence models, crash state enumeration, state names
internal/check/      materializing states, running checkers, the report
examples/kvstore/    toy store: two engines, six planted bugs, its checker
examples/sqlite/     SQLite workload (C) and checker (sh)
examples/matrix/     reruns the README tables and checks the verdicts
docker/              the dev image: Go, strace, SQLite
```

## License

MIT
