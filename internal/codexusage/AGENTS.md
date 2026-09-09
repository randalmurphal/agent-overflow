# Codex usage cache

This package coalesces Codex `account/usage/read` requests and caches by binary
and account ID. The account dimension prevents totals crossing a login switch.

The application supplies `Fetch`; wire decoding remains in
`internal/provider/codex`. Cache and share failures for `DefaultErrorTTL`
without converting them to empty success. Return deep defensive copies.
