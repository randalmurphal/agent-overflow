# Claude Code References

When touching `internal/provider/claude/` or any Claude-specific
behavior, use these as the source of truth — not guesses derived from
our own code.

## Reference Repos

- **Claude Code source** — local mirror at
  `/Users/randy/repos/claude-code-source-code/`.
  - TypeScript source for an older Claude Code release.
  - Useful when the installed `claude` binary's behavior is unclear
    and grepping minified strings is too noisy.
  - **Caveat:** the local copy is older than the installed binary in
    `~/.local/share/claude/versions/<version>` and may lag specific
    behaviors. Cross-check what you find against the installed
    binary's `strings` output before relying on it. Examples of
    drift seen in practice: the resume picker filter set
    (`utils/sessionStorage.ts:enrichLog`) gained additional rules
    in newer binaries; `initializeEntrypoint` (`main.tsx`) added
    overrides that rewrite preset env values.

- **Anthropic Claude Agent SDK** — `@anthropic-ai/claude-agent-sdk`.
  - Authoritative wire format and option shapes for stream-json
    invocations.
  - Read when the question is "what does the SDK actually send to
    the CLI?"

## Docs

- Claude Code overview: https://docs.claude.com/en/docs/claude-code
- Stream-JSON I/O reference: see `claude --help` (`--input-format`,
  `--output-format`, `--include-partial-messages`).

## Workflow

1. For wire shapes, start with `docs/references/claude-wire.md` —
   it pins canonical examples and known ambiguities.
2. For runtime behavior questions (how `--resume` filters, how the
   CLI decides entrypoint, how telemetry is tagged), grep the local
   `claude-code-source-code/src` first; it's faster than parsing the
   binary.
3. Confirm any source finding against the installed binary's
   strings if the area is one where drift has been observed.
4. If both sources disagree or are silent, follow
   `docs/references/spike-policy.md` and write an isolated spike.
