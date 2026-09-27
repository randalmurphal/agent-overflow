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
// Requests are stateless full parses. The one stateful path is Stream,
// which the backend's live highlighter holds for a code fence while an
// agent streams it: a full reparse per growth step costs O(n) each time,
// O(n^2) over the fence, while an incremental reparse and a query of the
// changed lines stays near constant.
package highlight
