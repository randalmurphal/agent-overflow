# internal/workflow/def/

Pure, engine-free workflow definition support. This package owns the YAML
authoring shape, resolver, embedded authoring schema, interpolation, generated
phase-envelope schemas, post-validation, and whole-graph dry-run validation.

## Boundaries

- Keep provider, engine, persistence, transport, scheduling, and profile loading
  out of this package.
- File I/O is limited to workflow YAML and sibling prompt files supplied by the
  caller.
- Workflow input names and phase IDs share one namespace. Phase output
  references use `phase.output`; nested object paths may continue after that.
- Validation returns typed findings and collects all independent errors.
- `Bindings` is intentionally narrow; profile loading implements it elsewhere.

## Files

| File | Responsibility |
|---|---|
| `types.go` | YAML and validation result types. |
| `parse.go` | Strict single-document YAML parsing. |
| `resolve.go` | Ordered scoped-directory resolution. |
| `schema.go` | JSON-Schema fragments and embedded authoring schema. |
| `envelope.go` | Generated control schema and payload post-validation. |
| `interpolate.go` | Single-pass prompt interpolation and template checks. |
| `validate*.go` | Whole-definition structural, graph, variable, and binding checks. |

Tests use deterministic fixtures under `testdata/` and must not inspect shared
application configuration.
