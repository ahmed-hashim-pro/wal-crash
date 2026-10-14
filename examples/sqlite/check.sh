#!/bin/sh
# The SQLite invariant, run by wal-crash inside a crash state's directory:
# the database opens (running SQLite's own recovery: hot-journal rollback
# or WAL replay), passes PRAGMA integrity_check, holds every acknowledged
# row, and holds no row that was never written.
db=${1:-test.db}

if ! ic=$(sqlite3 "$db" "PRAGMA integrity_check;" 2>&1); then
	echo "database does not open: $ic"
	exit 1
fi
if [ "$ic" != ok ]; then
	echo "integrity_check: $(echo "$ic" | grep -v '^\*\*\* in database' | head -1)"
	exit 1
fi
rows=$(sqlite3 "$db" "SELECT k || ' ' || v FROM kv;" 2>/dev/null) || rows=""

while read -r k v; do
	if ! printf '%s\n' "$rows" | grep -qxF "$k $v"; then
		echo "acknowledged row $k is missing"
		exit 1
	fi
done < "$WAL_CRASH_ACKED"

printf '%s\n' "$rows" | while read -r k v; do
	[ -z "$k" ] && continue
	if ! grep -qxF "PUT $k $v" "$WAL_CRASH_STDOUT"; then
		echo "row $k was never written"
		exit 1
	fi
done || exit 1
