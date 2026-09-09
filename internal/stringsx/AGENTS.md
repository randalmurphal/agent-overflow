# `internal/stringsx`

Small, dependency-free string helpers shared across packages. Keep functions general, allocation-conscious, and explicit about byte versus rune limits.

`Clip` is byte-oriented and may split UTF-8. Use `ClipRunes` or `TailRunes` when
text must retain rune boundaries. `SkipANSIEscape` returns just past a recognized
escape and resumes after the ESC on an unterminated CSI or OSC so it does not
swallow following text. Add helpers here only when more than one package needs
the same semantics.
