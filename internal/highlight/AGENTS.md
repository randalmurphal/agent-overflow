# `internal/highlight`

Tree-sitter syntax highlighting and compact span encoding for backend and persisted render paths.

- Raw source remains canonical. Persisted spans are a versioned cache and must be dropped when grammar, query, class mapping, or encoder versions change.
- Keep language detection deterministic and explicit. Add a grammar, aliases, query, class ids, and tests as one change.
- Bound source size, parse work, span count, nesting, and injected-language recursion before allocating heavily.
- Unknown languages, malformed patches, caps, and parser failures degrade to
  plain spans rather than failing the caller. Mark transient parse degradation
  `Incomplete` and never cache it; deterministic size truncation may be cached.
- Pool completed parsers only; trees belong to one parse and are closed after
  use. Close a timed-out or cancelled parser instead of returning it to the
  pool because upstream reset does not clear all cancelled state. No parser may
  be used concurrently.
- Preserve byte offsets and valid nesting through patch splitting and encoding.
- Seed and live highlighting must use the same version and class vocabulary as the frontend decoder.

Generated grammar bindings stay generated. Tests should cover malformed source, caps, injections, cache versioning, and encode/decode agreement.
