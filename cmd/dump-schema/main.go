// Tool: dump the fresh-from-source SQLite schema produced by running
// every migration in internal/store/migrate.go against an empty DB,
// or against a passed --db path. Used during the migration-squash work
// to diff against a long-lived production DB so drift becomes visible.
//
// Usage:
//   go run ./cmd/dump-schema > /tmp/fresh_schema.sql
//   go run ./cmd/dump-schema --db ~/.config/agent-overflow/agent-overflow.db > /tmp/live_schema.sql
//
// Throwaway. Delete after the squash lands.
package main

import (
	"database/sql"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"agent-overflow/internal/store"
)

func main() {
	dbFlag := flag.String("db", "", "path to existing sqlite db; if empty, build fresh from source via store.New")
	flag.Parse()

	dbPath := *dbFlag
	if dbPath == "" {
		dir, err := os.MkdirTemp("", "dump-schema-")
		if err != nil {
			fmt.Fprintln(os.Stderr, "mkdir:", err)
			os.Exit(1)
		}
		defer os.RemoveAll(dir)
		dbPath = filepath.Join(dir, "fresh.db")
		s, err := store.New(dbPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "store.New:", err)
			os.Exit(1)
		}
		s.Close()
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sql.Open:", err)
		os.Exit(1)
	}
	defer db.Close()

	rows, err := db.Query(`
		SELECT type, name, tbl_name, COALESCE(sql, '')
		FROM sqlite_master
		WHERE name NOT LIKE 'sqlite_%'`)
	if err != nil {
		fmt.Fprintln(os.Stderr, "query:", err)
		os.Exit(1)
	}
	defer rows.Close()

	type obj struct {
		Type, Name, Table, SQL string
	}
	var objs []obj
	for rows.Next() {
		var o obj
		if err := rows.Scan(&o.Type, &o.Name, &o.Table, &o.SQL); err != nil {
			fmt.Fprintln(os.Stderr, "scan:", err)
			os.Exit(1)
		}
		objs = append(objs, o)
	}
	if err := rows.Err(); err != nil {
		fmt.Fprintln(os.Stderr, "rows.Err:", err)
		os.Exit(1)
	}

	sort.SliceStable(objs, func(i, j int) bool {
		order := map[string]int{"table": 1, "index": 2, "trigger": 3, "view": 4}
		oi, oj := order[objs[i].Type], order[objs[j].Type]
		if oi != oj {
			return oi < oj
		}
		if objs[i].Table != objs[j].Table {
			return objs[i].Table < objs[j].Table
		}
		return objs[i].Name < objs[j].Name
	})

	for _, o := range objs {
		if o.SQL == "" {
			continue
		}
		fmt.Printf("-- %s %s (on %s)\n%s;\n\n", o.Type, o.Name, o.Table, o.SQL)
	}

	versions, err := db.Query(`SELECT version, name FROM migration_versions ORDER BY version`)
	if err != nil {
		fmt.Fprintln(os.Stderr, "versions query:", err)
		os.Exit(1)
	}
	defer versions.Close()
	fmt.Println("-- migration_versions")
	for versions.Next() {
		var v int
		var n string
		if err := versions.Scan(&v, &n); err != nil {
			fmt.Fprintln(os.Stderr, "versions scan:", err)
			os.Exit(1)
		}
		fmt.Printf("-- v%d %s\n", v, n)
	}
}
