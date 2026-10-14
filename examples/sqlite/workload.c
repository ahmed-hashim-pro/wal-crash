/* workload: insert N rows into an SQLite database, one transaction each,
 * printing "PUT k v" before a transaction and "ACK k v" once COMMIT has
 * returned, which is the moment SQLite promises the row is durable.
 *
 *   workload DB JOURNAL_MODE SYNCHRONOUS N
 *   workload test.db wal full 8
 */
#include <sqlite3.h>
#include <stdio.h>
#include <stdlib.h>

static sqlite3 *db;

static void exec(const char *sql) {
	char *err = 0;
	if (sqlite3_exec(db, sql, 0, 0, &err) != SQLITE_OK) {
		fprintf(stderr, "workload: %s: %s\n", sql, err);
		exit(2);
	}
}

int main(int argc, char **argv) {
	if (argc != 5) {
		fprintf(stderr, "usage: workload DB JOURNAL_MODE SYNCHRONOUS N\n");
		return 2;
	}
	int n = atoi(argv[4]);
	char sql[1024];
	if (sqlite3_open(argv[1], &db) != SQLITE_OK) {
		fprintf(stderr, "workload: open %s: %s\n", argv[1], sqlite3_errmsg(db));
		return 2;
	}
	snprintf(sql, sizeof sql, "PRAGMA journal_mode=%s; PRAGMA synchronous=%s;", argv[2], argv[3]);
	exec(sql);
	exec("CREATE TABLE IF NOT EXISTS kv(k TEXT PRIMARY KEY, v TEXT NOT NULL)");
	for (int i = 1; i <= n; i++) {
		/* A value long enough that each row lands on the page as real data. */
		char v[256];
		snprintf(v, sizeof v, "v%d-%0200d", i, i);
		printf("PUT k%d %s\n", i, v);
		fflush(stdout);
		snprintf(sql, sizeof sql, "BEGIN; INSERT INTO kv VALUES('k%d', '%s'); COMMIT;", i, v);
		exec(sql);
		printf("ACK k%d %s\n", i, v);
		fflush(stdout);
	}
	if (sqlite3_close(db) != SQLITE_OK) {
		fprintf(stderr, "workload: close: %s\n", sqlite3_errmsg(db));
		return 2;
	}
	return 0;
}
