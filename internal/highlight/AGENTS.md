# internal/highlight/

Server-side rendering for timeline content: markdown to HTML with
Chroma-highlighted fences, terminal output (ANSI) to HTML with color-class
spans. Frontend consumers paint the pre-rendered HTML via `{@html}`.

## Layout

- `doc.go` — package purpose line.
- `highlight.go` — `Renderer`, `Options`, `New`; the three public render
  methods (`RenderMarkdown`, `RenderANSI`, `RenderForKind`).
- `markdown.go` — goldmark + `goldmark-highlighting` + Chroma wiring
  (`chromahtml.WithClasses(true)`, `chromahtml.ClassPrefix("ch-")`).
- `ansi.go` — `buildkite/terminal-to-html/v3` wrapper.
- `dispatch.go` — `RenderForKind` table and exported `Kind*` string
  constants. The constants cover BOTH `items.kind` values (e.g.
  `KindAssistantText`, `KindThinking`) and `payloads.kind` values (e.g.
  `KindProposedPlan`, `KindCommandOutput`, `KindToolResult`) — the same
  dispatcher serves both write paths (item persist in triage + payload
  binding in `app.go`). `KindProposedPlan` is only reached via the
  payload-binding route (`GetPayloadData`/`GetPayloadPreview`); items
  of kind `proposed_plan` do not exist because the triage layer stores
  proposed plans as `tool_call` rows with a `proposed_plan` payload.
- `safelinks.go` — overrides goldmark's default AutoLink renderer with
  one that runs `IsDangerousURL` on the URL; without this,
  `<javascript:alert(1)>` et al. survive as live `href=`.
- `highlight_test.go` — plain/markdown/fence/oversize/ansi/unicode cases
  plus concurrent-safety smoke.
- `adversarial_test.go` — XSS and dangerous-URI cases kept separate so the
  security bar stays visible.

## Responsibility boundary

- What BELONGS here:
  - Pure-function rendering from raw text to an HTML fragment.
  - The input-size cap (`Options.MaxBytes`) and the escaped-`<pre><code>`
    fallback used when parsing is skipped or fails.
  - The single source of truth for which item/payload kinds are
    server-rendered (`RenderForKind`).
- What does NOT belong here:
  - Anything provider-specific. Kinds are plain strings; no imports
    from `internal/provider/*`.
  - Anything storage-aware. No `internal/store` import; callers pass
    raw text in and get a string out.
  - Caching. "No in-memory read models" applies — the cost is low enough
    that a content-hash cache would invent correctness bugs during
    streaming.

## Extension points

- To server-render a new kind: add a constant in `dispatch.go` and a case
  in `renderForKind`. Mirror the kind string with the value used in
  `internal/store`. Add a test in `TestRenderForKindDispatch`.
- To change the size cap, pass a non-zero `Options.MaxBytes` at
  construction. The default is defined by `defaultMaxBytes` in
  `highlight.go` and is exercised by `TestRenderMarkdownOversizedDefaultCap`.
- To change the Chroma style, pass a non-empty `Options.Style`. Output is
  class-based (`ch-*`), so styles only affect Chroma's CSS generator; the
  rendered HTML stays the same.

## Anti-patterns

- Do NOT return an error. Every render path has a fallback; callers
  should never branch on a nil/non-nil result.
- Do NOT build goldmark/chroma state inside a render method. Construct
  once in `New` so concurrent callers share immutable config.
- Do NOT import `internal/store`, `internal/triage`, or `main`. This
  package is a leaf.
- Do NOT disable goldmark's default HTML sanitization. The frontend uses
  `{@html}`; anything we emit is trusted.
- Do NOT add a content-hash cache. Root `CLAUDE.md` principle 4 plus
  streaming correctness make this a mistake.

## Testing

- Every render path needs a test for plain input, a feature-exercising
  input (bold, heading, fence with known lang, fence with unknown lang,
  SGR color), a malformed/partial input, and an oversize input.
- Adversarial tests live in `adversarial_test.go` and cover: raw
  `<script>`, `javascript:` link URIs, inline event handlers, and the
  equivalent payloads inside an ANSI color span. Any new rendering code
  path needs a matching adversarial case.
- Concurrency is covered by `TestRendererConcurrentUse`. `go test -race
  ./internal/highlight/...` must stay clean.

## References

- Root `CLAUDE.md` principle 4 ("frontend memory is bounded by the
  visible thread"); this package is why pre-rendered HTML is small and
  cheap to paint.
- `docs/references/spike-policy.md` — the goldmark and terminal-to-html
  APIs were verified by throwaway spikes before this package landed.
