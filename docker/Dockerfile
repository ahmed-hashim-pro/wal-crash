# The toolchain wal-crash is developed and tested with: Go, strace, and SQLite
# (CLI + headers for the SQLite example workload).
FROM golang:1.25
RUN apt-get update \
 && apt-get install -y --no-install-recommends strace sqlite3 libsqlite3-dev \
 && rm -rf /var/lib/apt/lists/*
