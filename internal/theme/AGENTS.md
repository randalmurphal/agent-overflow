# `internal/theme`

Loads user theme files and owns the atomic `appearance.json` selection in the
configuration directory. The frontend owns built-in themes, token parsing, and
application.

Go validates safe ids, regular-file access, per-file and aggregate size, and
result count, then returns file bytes as an opaque string. It must not parse
theme JSON or duplicate the frontend token vocabulary. Keep deterministic
ordering, skip symlinks, and preserve partial success with warnings for
eligible files that cannot be read.

Validate appearance mode, ids, and the optional cached native-window background
color. Empty selection fields resolve to package defaults. Boot refreshes the
generated schema and token reference while preserving user selection, and may
migrate the recognized legacy mode once.
