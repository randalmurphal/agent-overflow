// Package storetest hands tests a migrated SQLite store cloned from one
// template file, so a package with hundreds of store-backed tests replays the
// migration chain once instead of once per test, and a test process that
// clones nothing replays it not at all.
//
// internal/store builds its own template in store_test.go, since that file
// is package store and a helper package importing store cannot be imported
// back by it; both live in the run's fixture directory
// (internal/testutil/fixturedir), which owns their lifetime.
package storetest
