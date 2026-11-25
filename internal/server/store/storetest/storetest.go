// Package storetest hands tests a fresh server database: a SQLite file by
// default, or a new Postgres database when SEATIME_TEST_PG is set to a
// server URL, e.g.
//
//	SEATIME_TEST_PG='postgres://seatime@localhost:55432/postgres?sslmode=disable' go test ./internal/server/...
package storetest

import (
	"bytes"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// DSN is a new, empty database for one test (store.Open creates the
// schema): a file in t.TempDir(), or a Postgres database that is dropped
// when the test ends.
func DSN(t testing.TB) string {
	t.Helper()
	base := os.Getenv("SEATIME_TEST_PG")
	if base == "" {
		return filepath.Join(t.TempDir(), "t.db")
	}
	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("SEATIME_TEST_PG: %v", err)
	}
	admin, err := sql.Open("pgx", base)
	if err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 6)
	rand.Read(b)
	name := "seatime_test_" + hex.EncodeToString(b)
	if _, err := admin.Exec(`CREATE DATABASE ` + name); err != nil {
		admin.Close()
		t.Fatalf("SEATIME_TEST_PG: %v", err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(`DROP DATABASE IF EXISTS ` + name + ` WITH (FORCE)`); err != nil {
			t.Errorf("drop %s: %v", name, err)
		}
		admin.Close()
	})
	u.Path = "/" + name
	return u.String()
}

// Contents is everything stored in the database at dsn as raw bytes, for
// tests that check some text never reached it: the SQLite file and its
// write-ahead log, or every row of every Postgres table as text.
func Contents(t testing.TB, dsn string) []byte {
	t.Helper()
	if !strings.HasPrefix(dsn, "postgres://") && !strings.HasPrefix(dsn, "postgresql://") {
		var out []byte
		for _, f := range []string{dsn, dsn + "-wal"} {
			if raw, err := os.ReadFile(f); err == nil {
				out = append(out, raw...)
			}
		}
		return out
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT tablename FROM pg_tables WHERE schemaname='public'`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var n string
		rows.Scan(&n)
		tables = append(tables, n)
	}
	rows.Close()
	var out bytes.Buffer
	for _, tb := range tables {
		var s sql.NullString
		if err := db.QueryRow(`SELECT string_agg(x::text, E'\n') FROM "` + tb + `" x`).Scan(&s); err != nil {
			t.Fatal(err)
		}
		out.WriteString(s.String + "\n")
	}
	return out.Bytes()
}
