# Third-party notices

This directory vendors syntax-highlighting queries and, for languages
without an upstream Go module, generated grammar sources. Each
language directory's `UPSTREAM` file records exact provenance
(repository, revision, applied reconciliations).

## Highlight and injection queries (`*/highlights.scm`, `*/injections.scm`)

Copied from [helix-editor/helix](https://github.com/helix-editor/helix)
`runtime/queries/`, pinned per `UPSTREAM`. Licensed under the
**Mozilla Public License 2.0** — each directory carries the full text
as `LICENSE`. Modifications (inherits expansion, version-skew
reconciliations) are noted in the adjacent `UPSTREAM` file, satisfying
MPL-2.0 §3.4 modification notice.

## Grammar Go modules (linked, not vendored)

All **MIT**:

- github.com/tree-sitter/tree-sitter-{python,go,typescript,javascript,json,bash,css,html,rust,c,cpp,java}
- github.com/tree-sitter-grammars/tree-sitter-yaml
- github.com/DerekStride/tree-sitter-sql (pinned by commit pseudo-version)
- github.com/tree-sitter/go-tree-sitter (runtime, MIT)

## Vendored grammar sources (`*/src/`)

All **MIT**, full text in each directory's `LICENSE-grammar`:

- `svelte/src/` — [themixednuts/tree-sitter-htmlx](https://github.com/themixednuts/tree-sitter-htmlx)
- `markdown/src/`, `markdown-inline/src/` — [tree-sitter-grammars/tree-sitter-markdown](https://github.com/tree-sitter-grammars/tree-sitter-markdown)
- `diff/src/` — [the-mikedavis/tree-sitter-diff](https://github.com/the-mikedavis/tree-sitter-diff)

The `src/` trees are generated parser output (`parser.c`) plus
hand-written scanners and headers, marked `linguist-generated` in
`.gitattributes`.
