// Package ini binds the pinned INI parser; see UPSTREAM.
package ini

// #cgo CFLAGS: -Isrc -std=c11
// #include "src/parser.c"
import "C"

import "unsafe"

func Language() unsafe.Pointer { return unsafe.Pointer(C.tree_sitter_ini()) }
