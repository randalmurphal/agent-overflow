// Package diff is the hand-written Go binding for the vendored
// tree-sitter-diff grammar (no upstream Go module exists). See
// UPSTREAM for the pinned revision.
package diff

// #cgo CFLAGS: -Isrc -std=c11
// #include "src/parser.c"
import "C"

import "unsafe"

// Language returns the raw grammar pointer for
// tree_sitter.NewLanguage.
func Language() unsafe.Pointer {
	return unsafe.Pointer(C.tree_sitter_diff())
}
