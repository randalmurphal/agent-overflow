# `internal/spinner`

Loads user spinner sprite pairs from the configuration directory. Settings store selection and exclusions; the frontend owns built-in sprites and rendering.

A sprite is an opaque `.json` manifest and `.png` byte pair sharing a valid id.
Go validates pairing, safe regular-file access, per-file and aggregate sizes,
and result count. It deliberately does not parse the manifest or PNG; the
frontend owns animation and image validation. Return warnings alongside usable
sprites so one broken custom asset does not erase the catalog.

The PNG crosses the binding as bounded base64 by design. Keep deterministic ordering and reject symlink or containment escapes from the spinner directory.
