// Package make binds the pinned vendored tree-sitter grammar. See UPSTREAM.
package make

// #cgo CFLAGS: -Isrc -std=c11
// #include "src/parser.c"
import "C"

import "unsafe"

// Language returns the raw grammar pointer for tree_sitter.NewLanguage.
func Language() unsafe.Pointer {
	return unsafe.Pointer(C.tree_sitter_make())
}
