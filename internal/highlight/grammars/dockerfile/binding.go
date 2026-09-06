// Package dockerfile binds the pinned vendored tree-sitter grammar. See UPSTREAM.
package dockerfile

// #cgo CFLAGS: -Isrc -std=c11
// #include "src/parser.c"
// #include "src/scanner.c"
import "C"

import "unsafe"

// Language returns the raw grammar pointer for tree_sitter.NewLanguage.
func Language() unsafe.Pointer {
	return unsafe.Pointer(C.tree_sitter_dockerfile())
}
