// Package markdown is the hand-written Go binding for the vendored
// tree-sitter-markdown block grammar (no upstream Go module exists).
// See UPSTREAM for the pinned revision.
package markdown

// #cgo CFLAGS: -Isrc -std=c11
// #include "src/parser.c"
// #include "src/scanner.c"
import "C"

import "unsafe"

// Language returns the raw grammar pointer for
// tree_sitter.NewLanguage.
func Language() unsafe.Pointer {
	return unsafe.Pointer(C.tree_sitter_markdown())
}
