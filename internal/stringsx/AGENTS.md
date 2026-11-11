# `internal/stringsx`

Small, dependency-free string helpers shared across packages. Keep functions general, allocation-conscious, and explicit about byte versus rune limits.

`Clip` is byte-oriented and may split UTF-8. Use `ClipRunes` or `TailRunes` when
text must retain rune boundaries. `SkipANSIEscape` returns just past a recognized
escape and resumes after the ESC on an unterminated CSI or OSC so it does not
swallow following text. `FoldForFind` is the find bar's match rule (the CI log
search and a pane's thread find); the frontend's `foldFindText` implements it
too, so change both together with the shared vectors in
`frontend/src/test/fixtures/findVectors.json`. Both fold by one checked-in table
generated from package `unicode` (`findfold_table.go`,
`frontend/src/lib/utils/findFoldTable.ts`); after a Go upgrade moves the Unicode
version, regenerate both with `FIND_FOLD_TABLE_UPDATE=1 make go-test
GO_TEST_PKGS=./internal/stringsx`. Add helpers here only when
more than one package needs the same semantics.
