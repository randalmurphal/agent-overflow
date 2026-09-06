# Third-party notices

This directory vendors syntax-highlighting queries and, for languages
without a usable upstream Go module, generated grammar sources. Each
language directory's `UPSTREAM` file records exact provenance
(repository, revision, applied reconciliations).

## Highlight and injection queries (`*/highlights.scm`, `*/injections.scm`)

Most are copied from [helix-editor/helix](https://github.com/helix-editor/helix)
`runtime/queries/`, pinned per `UPSTREAM`. Licensed under the
**Mozilla Public License 2.0** — each directory carries the full text
as `LICENSE`. Modifications (inherits expansion, version-skew
reconciliations) are noted in the adjacent `UPSTREAM` file, satisfying
MPL-2.0 §3.4 modification notice.

The `toml`, `dockerfile`, `make`, `xml`, and `powershell` queries come from
their grammar upstreams under **MIT**. The `ini` query comes from its grammar
upstream under **Apache-2.0**. Their `LICENSE` and `UPSTREAM` files carry the
license text, revision, and any query reconciliations.

## Grammar Go modules (linked, not vendored)

**MIT**:

- github.com/tree-sitter/tree-sitter-{python,go,typescript,javascript,json,bash,css,html,rust,c,cpp,java}
- github.com/tree-sitter-grammars/tree-sitter-yaml
- github.com/tree-sitter-grammars/tree-sitter-toml
- github.com/tree-sitter-grammars/tree-sitter-xml
- github.com/airbus-cert/tree-sitter-powershell
- github.com/DerekStride/tree-sitter-sql (pinned by commit pseudo-version)
- github.com/tree-sitter/go-tree-sitter (runtime, MIT)

**Apache-2.0**:

- github.com/tree-sitter-grammars/tree-sitter-hcl (`hcl/LICENSE-grammar`)

## Vendored grammar sources (`*/src/`)

**MIT**, full text in each directory's `LICENSE-grammar` or the shared
grammar/query `LICENSE` named by `UPSTREAM`:

- `svelte/src/` — [themixednuts/tree-sitter-htmlx](https://github.com/themixednuts/tree-sitter-htmlx)
- `markdown/src/`, `markdown-inline/src/` — [tree-sitter-grammars/tree-sitter-markdown](https://github.com/tree-sitter-grammars/tree-sitter-markdown)
- `diff/src/` — [the-mikedavis/tree-sitter-diff](https://github.com/the-mikedavis/tree-sitter-diff)
- `dockerfile/src/` — [camdencheek/tree-sitter-dockerfile](https://github.com/camdencheek/tree-sitter-dockerfile)
- `make/src/` — [alemuller/tree-sitter-make](https://github.com/alemuller/tree-sitter-make)

**Apache-2.0**, full text in `ini/LICENSE-grammar`:

- `ini/src/` — [justinmk/tree-sitter-ini](https://github.com/justinmk/tree-sitter-ini)

The `src/` trees contain generated parser output (`parser.c`) plus
hand-written scanners and headers. `.gitattributes` marks generated
`parser.c` files, not the hand-written scanners, as `linguist-generated`.
