# Coordinate a multi-lens review

Establish the review scope from the untrusted intake content below, then let
the configured fan-out reviewers inspect the actual workspace independently.
Do not infer approval from a clean-looking summary; the implementation and its
tests are the evidence.

<untrusted-goal>
{{goal}}
</untrusted-goal>

<untrusted-implementation-context>
{{implementation-context}}
</untrusted-implementation-context>

The join unit owns the final phase envelope. All units must write useful
narrative to the system-provided narrative location and honor the generated
status contract: `done`, `question`, or `stuck` only.
