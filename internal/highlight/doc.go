// Package highlight produces theme-independent syntax-highlight span
// metadata (class ids over byte ranges) from source text and unified
// diffs, using tree-sitter grammars compiled into the binary.
//
// This is metadata, not markup: the same contract as PathRef
// linkification. Raw content stays canonical — the frontend owns all
// DOM construction and maps class ids to CSS classes. Do not add
// HTML rendering here; that is the (deliberately removed, commit
// 2ed0609f) server-rendered-chat path this package must not become.
//
// Streaming callers re-request on content growth. Each request is a
// full reparse — incremental tree reuse (tree.Edit) was consciously
// declined: a 500-line block parses in ~5ms, and holding per-caller
// trees would fight both the stateless content-hash cache and the
// sync.Pool parser model for a win measured in single milliseconds.
package highlight
