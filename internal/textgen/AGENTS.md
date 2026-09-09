# Provider text generation

This package runs short structured-output jobs through provider CLIs. It owns
subprocess invocation, scratch files, output bounds, redaction, and generic JSON
post-processing. Callers own prompts, schemas, budgets, provider choice, and
task-specific decoding.

Every subprocess environment goes through `provider.BuildEnvironment`.
`ProcessOutputLimit` and `JSONOutputLimit` bound different channels and must
remain separate. `LimitPromptSection` clips on rune boundaries even though its
budget is measured in bytes.

`TranslateCLINotFound` preserves `context.DeadlineExceeded` through
`errors.Is`; fallback logic retries timeouts but not cancellation.
`RedactError` removes provider stderr because it may repeat prompts, paths, or
environment values.

Keep the executor seam generic. Do not add task-specific helpers or schema
types here. Tests must inject executors and never invoke real provider binaries.
