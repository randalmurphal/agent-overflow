# internal/promptoverride/

The pure half of the settings-level system-prompt override
(`docs/specs/prompt-tool-overrides.md`): pick the entry that applies to a
session, and render its placeholders against spawn-time facts.

```go
entry, ok := promptoverride.Match(settings.PromptOverridesForProvider(t.Provider), t.Provider, t.Model)
prompt := promptoverride.Render(entry.Prompt, promptoverride.Facts{WorkDir: ..., ...})
```

Gathering the facts is NOT this package's job — that needs git subprocesses,
the model catalog, and the host probe, so it lives in
`app_session_prompt_override.go`. What lives here is everything that is
decidable from data alone, which is what keeps the matching and substitution
rules testable without an `*App`.

## The placeholder list is closed

`Render` substitutes the eight `Token*` constants and nothing else, in ONE
left-to-right pass:

- An unknown or misspelled placeholder (`{{WORKDIRR}}`, `{{ WORKDIR }}`)
  survives into the prompt verbatim. That is the user's decision recorded in
  the spec: a typo the user can see in the model's context beats a silent
  deletion, and beats failing the spawn over prompt text.
- An absent fact renders as EMPTY, never as the raw token. A model reading a
  literal `{{GIT_BLOCK}}` is worse off than one reading a blank section.
- A substituted value is never rescanned, so a workspace path that happens to
  contain a token cannot expand further.

Adding a token means adding it to the constants, to `Render`, to `Facts`, to
the app-layer fact gatherer (`promptOverrideFacts`), and to the render table
test — plus the frontend's insert menu, which is not optional:
`TestPlaceholderTokensMatchTheFrontendMirror` parses
`frontend/src/lib/utils/promptOverrides.ts` and fails on drift in either
direction. A token only the legend knows renders literally into the model's
context; a token only Go knows is a substitution nobody is told about.

## Matching is by normalized model slug

`Match` normalizes both sides through `provider.NormalizeModelSlug` and
nothing else. That function is also where the `[1m]` marker is trimmed — for
Claude and claude-tui only — so an entry saved as `claude-opus-5` matches a
thread launched on `claude-opus-5[1m]`, and an alias (`opus`) matches its
resolved id. Without that, a user who switches a thread to the 1M tier
silently loses their override.

Do NOT re-apply `provider.TrimContextMarker` on the way in, which this
package did until it was removed: the marker rule is Claude's, and layering
it on top applied it to CODEX ids too — a bracketed codex id would be
trimmed on this one path and nowhere else in the app, so an entry could
match here and miss everywhere the same id is compared. The provider package
owns which providers the rule covers.

Entries are evaluated in the user's order and the first enabled entry listing
the model wins. Disabled entries and entries with a blank prompt are skipped
rather than matched-and-ignored, so a blank draft never shadows a working entry
further down the list — `TestMatchWalksPastABlankDraftToAWorkingEntry` pins
both skip reasons, because turning either `continue` into a return otherwise
passes the whole suite.

## `ClaudeMemoryDir` does not encode the slug itself

It delegates to `sessionfork.WorkspaceProjectDir`, the same encoder the
session-relocation writer uses. One encoder means the directory AO creates and
the directory a resumed CLI reads cannot drift.

`ok == false` is a real answer, not an error: past
`sessionfork.MaxSanitizedSlugLen` the CLI appends a `Bun.hash` suffix Go cannot
reproduce, so the exact directory is unknowable and the caller must degrade
(render the placeholder empty, create nothing) rather than create a
plausible-looking wrong directory. A hard error means the workspace path could
not be canonicalized.

Creating the directory is deliberately elsewhere: `Render` is called on the
live-config reconcile path as well as the spawn path, so nothing here may have
a side effect. `App.ensureClaudeMemoryDir` does the `MkdirAll`, spawn-only.

## Anti-patterns

- Do NOT gather facts here. No subprocesses, no `os` reads beyond the
  path canonicalization `ClaudeMemoryDir` delegates — this package must stay
  cheap enough to call on every reconcile.
- Do NOT make an unknown placeholder an error. It is user prose, not syntax.
- Do NOT introduce a second render pass or a regex-based one. The single-pass
  `strings.NewReplacer` is what makes "a fact is never rescanned" structural.
