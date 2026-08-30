# Spike Policy

When the behavior of an external tool (Claude Code, Codex, Wails, a
library, an OS syscall) is unclear, **don't guess and don't infer from
what this repo happens to do**. Write an isolated spike test, confirm
the behavior, then port the learning back.

## Why

Our code may already encode a wrong assumption. Reading it to decide
what "should" happen re-entrenches the bug. The external tool is the
ground truth.

## How

1. Create a throwaway directory under `/tmp` (or `$TMPDIR`).
2. Write the smallest possible program that exercises the behavior
   in isolation: no agent-overflow code, no test harness, just the
   external tool.
3. Run it against the real external process (or library). Record
   inputs and outputs.
4. Once the behavior is clear, close the loop in this repo with:
   - A code change, if needed.
   - A test that would have caught the old assumption.
   - A one-liner comment only if the behavior is non-obvious and a
     future reader would be surprised.
5. Discard the spike. Don't check it into this repo.

## When to Spike

- You can't find the answer in the reference repos (the Claude Code
  source, codex-source, CodexMonitor).
- The repos disagree with each other.
- A protocol message in production doesn't match what the parser
  expects.
- You're about to add a workaround for something you can't explain.

## What Not to Do

- Don't copy-paste from the reference repos without confirming the
  behavior still holds in our environment.
- Don't add retries, sleeps, or tolerance to cover for behavior you
  haven't verified.
- Don't leave the spike code in this repo "for later". It becomes
  dead code that future readers treat as authoritative.
