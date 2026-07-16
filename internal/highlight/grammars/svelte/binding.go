// Package svelte is the hand-written Go binding for the vendored
// svelte grammar (themixednuts/tree-sitter-htmlx — the grammar helix
// pins, which its svelte queries target). No upstream Go module
// exists; see UPSTREAM for the pinned revision.
package svelte

// #cgo CFLAGS: -Isrc -std=c11
// #include "src/parser.c"
// #include "src/scanner.c"
import "C"

import "unsafe"

// Language returns the raw grammar pointer for
// tree_sitter.NewLanguage.
func Language() unsafe.Pointer {
	return unsafe.Pointer(C.tree_sitter_svelte())
}
