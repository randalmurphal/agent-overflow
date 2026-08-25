// Package storetest hands tests a migrated SQLite store cloned from one
// template file, so a package with hundreds of store-backed tests replays the
// migration chain once instead of once per test.
//
// internal/store keeps its own private copy of this pattern in store_test.go:
// that file is package store, and a helper package importing store cannot be
// imported back by it without an import cycle.
package storetest
