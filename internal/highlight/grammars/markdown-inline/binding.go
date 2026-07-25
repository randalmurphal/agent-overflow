// Package markdowninline is the hand-written Go binding for the
// vendored tree-sitter-markdown inline grammar — the second half of
// the split markdown parser, injected into block-level inline content.
// See UPSTREAM for the pinned revision.
package markdowninline

// #cgo CFLAGS: -Isrc -std=c11
// #include "src/parser.c"
// #include "src/scanner.c"
import "C"

import "unsafe"

// Language returns the raw grammar pointer for
// tree_sitter.NewLanguage.
func Language() unsafe.Pointer {
	return unsafe.Pointer(C.tree_sitter_markdown_inline())
}
