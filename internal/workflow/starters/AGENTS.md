# internal/workflow/starters/

Embedded product-quality workflow definition sets used only as sources for
`ao workflow new`. The workflow engine must never load this package as a hidden
built-in definition tier.

Each starter directory contains one `workflow.yaml` plus its sibling prompt
Markdown files. Keep the leading binding inventory in the YAML synchronized
with every check, command, and capacity used by the definition; tests enforce
the exact match and validate the complete copied set through `workflow/def`.
