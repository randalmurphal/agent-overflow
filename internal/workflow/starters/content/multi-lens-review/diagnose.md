# Diagnose the post-review validation failure

Inspect the check evidence and workspace without modifying it. Classify the
failure as an implementation defect, a flaky check, or infrastructure failure,
then provide a causal diagnosis and the narrowest useful remedy. Do not label a
failure flaky merely because a retry might be convenient.

<untrusted-check-details>
{{validate.details}}
</untrusted-check-details>

Write the narrative to the system-provided path. Finish with the generated
`done` / `question` / `stuck` envelope; on `done`, provide
`outputs.classification` (`genuine`, `flaky`, or `infrastructure`),
`outputs.diagnosis`, and `outputs.remedy`.
