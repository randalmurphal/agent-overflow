package store

import (
	"database/sql"
	"fmt"
	"strings"
)

// sqlQueryer is the read half both *sql.DB and *sql.Tx satisfy. Read
// helpers take one instead of reaching for s.reader() so a caller that
// needs several reads to describe ONE consistent state — SyncThreadWindow
// attesting its stamps against the rows it returns — can run them inside a
// single read-pool transaction (WAL snapshot isolation) without a second
// copy of the query logic existing.
type sqlQueryer interface {
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}

// placeholders renders `?,?,?` for an `IN (...)` clause of count binds.
// Empty for count <= 0 — callers must short-circuit before building a
// query with an empty IN list rather than relying on SQLite's parse
// error to tell them.
func placeholders(count int) string {
	if count <= 0 {
		return ""
	}
	return strings.TrimRight(strings.Repeat("?,", count), ",")
}

// uniqueNonEmptyStrings is placeholders' companion: the caller-supplied
// id lists that feed an `IN (...)` clause arrive from the wire, so they
// are trimmed, de-duplicated, and stripped of blanks before their length
// is used as both the bind count and the expected rows-affected count. A
// duplicate id would otherwise make an exactly-correct UPDATE look short.
func uniqueNonEmptyStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

// UniqueNonEmptyStringsForApp exposes uniqueNonEmptyStrings to the app
// layer, which normalises the same wire-supplied id lists before it calls
// in — so the count it validates against a limit is the count the store
// will use.
func UniqueNonEmptyStringsForApp(values []string) []string {
	return uniqueNonEmptyStrings(values)
}

func requireRowsAffected(result sql.Result, action string) error {
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("%s: rows affected: %w", action, err)
	}
	if rows == 0 {
		return fmt.Errorf("%s: %w", action, sql.ErrNoRows)
	}
	return nil
}
