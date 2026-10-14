# Recorded with `make demo` (VHS v0.10.0, inside its container).
Output docs/demo.gif
Set Shell bash
Set FontSize 13
Set Width 1440
Set Height 640
Set Padding 18
Set TypingSpeed 15ms
Set WaitTimeout 120s
Set WaitPattern /^\$ ?$/
Set Theme "GitHub Dark"

Hide
Type "export PATH=$PWD/bin:$PATH kv=$PWD/bin/kvstore PS1='$ ' && clear"
Enter
Wait
Show

Type "# A log store that acknowledges a put before fsyncing it. Killing the process never loses it; a power cut can:"
Enter
Type `wal-crash run --model all --checker "$kv -engine log -bug ack-before-fsync -dir . check" -- $kv -engine log -bug ack-before-fsync -dir {dir} load 5`
Enter
Wait
Sleep 5s

Type "clear"
Enter
Type "# SQLite in rollback-journal mode with synchronous=FULL:"
Enter
Type "wal-crash run --checker $PWD/examples/sqlite/check.sh -- sqlite-workload {dir}/test.db delete full 6"
Enter
Wait
Sleep 5s
Type "# ...and with synchronous=EXTRA, which also fsyncs the directory after deleting the journal:"
Enter
Type "wal-crash run --checker $PWD/examples/sqlite/check.sh -- sqlite-workload {dir}/test.db delete extra 6 | tail -1"
Enter
Wait
Sleep 5s
