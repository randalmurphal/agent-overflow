# DRAFT — upstream bug report for anthropics/claude-code

> Status: **draft, not filed**. File on maintainer go-ahead. Sanitized
> fixture lives at
> [`fixtures/claude/session_api_error_offbranch.jsonl`](fixtures/claude/session_api_error_offbranch.jsonl).
> Internal context: invariant 28, claude-wire.md §"deferred
> system/api_error rows", incident 2026-06-10.

---

**Title**: Deferred `system/api_error` session rows are written with a
stale `parentUuid`, silently dropping the prior turn's tail from every
resume and hard-failing `--resume-session-at`

**Versions observed**: 2.1.167, 2.1.168, 2.1.170 (rows written by all
three; resume failure reproduced on 2.1.170). Dates: rows captured
2026-06-08 → 2026-06-10; behavior verified 2026-06-10.

## Summary

When an API request inside a turn fails transiently and is retried
(`system/api_retry` on the stream; the turn then completes normally),
the CLI does not write the corresponding `system/api_error` rows to the
session JSONL at retry time. It buffers them and appends them **at the
next user send** — but with `parentUuid` pointing at the transcript
leaf from *retry time* (a mid-turn row), not the file's current leaf.
The rest of the turn — everything after the retry point, including the
final assistant message — is bypassed in the parent graph.

Because session-resume context is reconstructed by walking `parentUuid`
from the file's last uuid-bearing transcript row, and the next user row
chains onto the api_error rows, this has two user-visible consequences:

1. **Every cold `--resume` of an affected session silently drops the
   prior turn's tail** (often the final assistant answer) from the
   model's context. No error — the model just doesn't remember the end
   of its own previous turn.
2. **`--resume <id> --resume-session-at <uuid>` hard-fails for any
   bypassed row** (e.g. the turn's final assistant message — exactly
   the uuid an SDK client would pass):
   ```json
   {"type":"result","subtype":"error_during_execution","is_error":true,
    "num_turns":0,
    "errors":["No message found with message.uuid of: <uuid>"]}
   ```
   After emitting this pre-init result the process does not exit; it
   lingers until killed.

## Row shape (sanitized, verbatim fields)

```json
{"type":"system","subtype":"api_error","level":"error",
 "uuid":"<err1>",
 "parentUuid":"<MID-TURN row from retry time — stale>",
 "retryAttempt":1,"retryInMs":1000,"maxRetries":10,
 "error":{"message":"Connection error.","connection":{"code":"ECONNRESET"}},
 "content":"API error","timestamp":"<NEXT-SEND time, not retry time>"}
```

Multiple retries chain onto each other (`err2.parentUuid = err1`), and
the next user send's row chains onto the last of them
(`u3.parentUuid = err2`), entrenching the bypass.

## Reproduction

1. Run a turn that hits a transient API failure mid-turn (ECONNRESET /
   529 with successful retry). The turn completes normally on the wire.
   The session JSONL at this point is healthy — no api_error rows.
2. Send any next message. The api_error rows are appended now, with
   `parentUuid` pointing at the mid-turn row from step 1, followed by
   the new user row chained onto them.
3. `claude --resume <session>` → the tail of turn 1 (post-retry-point
   rows, including the final assistant message) is absent from resumed
   context.
4. `claude --resume <session> --resume-session-at <final-assistant-uuid>`
   → pre-init `error_during_execution` ("No message found with
   message.uuid of: ..."), lingering process.

The checked-in 8-row fixture reproduces the topology directly: drop it
into `~/.claude/projects/<slug>/<id>.jsonl` and run step 4 against
`a3-final`.

## Bisect — the stale parent is the whole bug

Performed on a real affected session (2026-06-10, 2.1.170), mutating
only the api_error rows and attempting `--resume-session-at` the
turn's final assistant row:

| Mutation | Resume result |
|---|---|
| File unchanged (stale parents) | **FAILS** — "No message found with message.uuid of: ..." |
| api_error rows deleted | works |
| api_error rows kept, `parentUuid` re-chained to file predecessor | works |

## Expected behavior

Either of:
- Write api_error rows at retry time (their parent is then genuinely
  the current leaf), or
- When writing deferred rows, parent them on the file's **current**
  leaf rather than the buffered retry-time leaf.

The rows themselves are useful diagnostics — only the stale parent is
harmful.

## Impact

Any client that resumes sessions by uuid (the Python agent SDK's
`resume_session_at`, any wrapper UI tracking transcript leaves) is
affected: the failure appears one full turn AFTER the network blip that
caused it, on a session whose wire stream showed a successful turn, and
the resulting error message names a uuid that demonstrably exists in
the file — just off the parentUuid branch. Recovery requires manually
re-chaining the JSONL.
