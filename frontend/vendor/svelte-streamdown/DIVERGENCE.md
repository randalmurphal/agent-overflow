# svelte-streamdown divergence ledger

Every entry below is permanent in-tree divergence from upstream 3.1.2, and
this file is the only record of it. Wiring, build-config couplings and the
upstream-diff recipe are in [`VENDOR.md`](VENDOR.md); the decision rule for
`patches/` vs `vendor/` is in [`frontend/AGENTS.md`](../../AGENTS.md).

`vendor/svelte-streamdown/` is `svelte-streamdown@3.1.2` with the former
13-hunk pnpm patch applied in place; the patch file and its
`patchedDependencies` entry are gone. Upstream is dormant, so every hunk was
permanent divergence being re-rolled by hand on each bump for no benefit.

**The rule for a markdown-pipeline bug is: fix it in this tree, add or extend
its entry below with the regression test that pins it, and open an upstream PR
when the fix is a general bug rather than a deliberate deviation.** Adding a
*new* divergence needs the same justification a patch hunk did — this tree is
a maintenance liability in proportion to how far it drifts. Fixes that belong
in our own code (`src/lib/markdown/`, `ChatMarkdown.svelte`) still belong
there; vendoring is not permission to move app logic into the library.

Paths below are relative to `dist/`. Regression-test paths are relative to
`frontend/src/lib/`.

1. **`$`-prose guard** (`marked/marked-math.js`,
   `singleDollarLooksLikeProse`) — a bare single `$` only opens inline
   math when its closer is not an identifier char and its content holds
   no backtick; otherwise agent prose like `$ref … $foo` renders as
   serif KaTeX. Regression: `components/chat/AssistantMessage.test.ts`.
2. **lexer-rule overrides** (`marked/index.js`, top-of-file) — require
   `~~` for strikethrough (single `~` false-positives on `~240MB`),
   disable mailto autolinking (`composer@0.7s` → bogus `mailto:`), and
   split the GFM text rule's leading ``[`~]+`` run so a `~` cannot
   swallow an adjacent backtick. Composes with upstream's
   bottom-of-file options cache + incremental `parseBlocks`.
3. **deferred-typesetting hosts** (`Elements/{Math,Mermaid}`,
   `context`, `Streamdown`) — `registerAsyncResource` /
   `pendingAsyncCount` / `onsettled` let a Streamdown signal when its
   async work (katex, mermaid, backend highlight spans) has settled,
   so the committed-prefix vs volatile-tail two-instance split in
   `ChatMarkdown.svelte` defers math/mermaid typesetting off the
   streaming tail. Our own `StreamdownCodeHost` participates through
   the same context hooks (no `Code.svelte` change needed — the
   library's shiki Code component is unused and tree-shaken).
   Orthogonal to upstream's parse cache — they stack.
4. **completion-disable** (`utils/parse-incomplete-markdown.js`) — a
   trailing `.filter` drops the 10 inline emphasis/code/math
   speculative completers (they mis-close on lone delimiters
   mid-stream); the structural completers (links, citations,
   footnotes, fences, MDX) keep upstream's behavior. Re-enabling the
   safe ones (bold/italic/strike) is a separate follow-up. The
   isWithinInlineCode guards that used to patch the dropped
   completers were removed as dead code when the patch was re-rolled
   onto 3.1.2; recover both from git history
   (`patches/svelte-streamdown@3.1.2.patch`, deleted at the vendoring
   commit) if the re-enable follow-up happens.
5. **subscript range-guard** (`marked/marked-subsup.js`) — `subRule`'s
   closing `~` carries a `(?!\d)` lookahead so approximate-range prose
   like `~5~10` / `~50~100` no longer tokenizes its low bound as
   `<sub>5</sub>`. Legit subscript (`H~2~O`, closing `~` before a
   letter) is unaffected. `supRule` (`^N^M`) is deliberately left
   unguarded — carets are rare in agent prose and none was reported;
   revisit if `^5^10`-style superscript false-positives surface.
   Regression: `AssistantMessage.test.ts`. Both sub/sup rules also
   exclude backticks from their content (code spans bind tighter, so
   a delimiter inside an inline code span can no longer open sub/sup
   across it).
6. **setext-dash-guard** (`utils/parse-incomplete-markdown.js`,
   `stripDanglingSetextUnderline`) — mid-stream a nested bullet's marker
   arrives a chunk before its text, so the volatile tail momentarily ends
   in `…text:\n  -`; CommonMark reads that lone `-`/`=` run under a
   non-blank line as a Setext underline and balloons the line above to
   `<h2>`/`<h1>` for one chunk (font + margin + re-wrap), collapsing back
   the next chunk. The guard drops a trailing lone `^[ \t]*[-=]+[ \t]*$`
   line when the line above is non-blank, applied AFTER
   `defaultParser.parse` so an open fence is sealed first (a `-` inside it
   is no longer the last line). Streaming volatile-tail only: prefix and
   settled instances pass `parseIncompleteMarkdown === false`, so a
   *settled* Setext heading still renders (a streamed one defers one
   chunk — rare; agent prose uses ATX `#`). No upstream issue filed —
   a general svelte-streamdown streaming bug, still an upstream-PR
   candidate. Full rationale + the blank-above safety net are in the
   source comment. Regression: `AssistantMessage.test.ts`.
7. **split-instance parse bypass** (`Block.svelte`) — honors
   `parseIncompleteMarkdown === false` from props; upstream types the
   prop but never reads it in Block, so the committed-prefix and
   settled instances in `ChatMarkdown.svelte` couldn't opt out of
   speculative completion without this. Trivial upstream-PR candidate;
   revert to upstream's Block if it ever honors the prop.
8. **block-math flexible form** (`marked/marked-math.js`, `blockRule`
   alt 3) — accepts `$$ CONTENT $$` where the content opens with a
   space (not a newline) and contains internal newlines, e.g.
   LLM-emitted matrices (`$$ \begin{pmatrix}…`). Without it those
   blocks fell through to paragraph rendering mid-stream ("math
   starts to render then turns back into raw markdown"). Closing `$$`
   must be followed by newline/end-of-string so adjacent same-line
   inline `$$X$$` pairs still take the single-line alternative.
9. **typeset caches** (`Elements/Math.svelte`,
   `Elements/Mermaid.svelte`) — module-level KaTeX HTML cache
   (deterministic output, LRU cap 500); Mermaid SVG cache keyed on
   `(theme, sanitized source)` with a per-insertion uniqueId rewrite
   so two in-DOM instances of the same diagram can't collide on
   document-scoped `url(#…)`/`xlink:href` ids (LRU cap 100). Both
   exist because the committed-prefix/volatile-tail split remounts
   each settled block once — the caches make that migration free.
   (Code blocks get the same treatment from our own
   `markdown/codeSpanCache.ts`, in app code.) Perf-only: each can be
   dropped independently if upstream grows an equivalent. The mermaid
   key has since been widened past `theme` — see entry 18.
10. **relative-reference links** (`Elements/Link.svelte`) — the
    blocked-link branch drops its " [blocked]" suffix for schemeless
    relative references (`docs/guide.md`, `../x`, `#frag` — common
    repo-relative links in PR/issue bodies). They aren't navigable
    here but they aren't *blocked URLs* either; the text renders
    muted with the href as hover title. Disallowed absolute URLs
    (`javascript:` etc.) and network-path refs (`//host/x`) keep the
    tag. Regression: `ChatMarkdown.test.ts`. Upstream-PR candidate.
11. **fence-seal fidelity** (`utils/parse-incomplete-markdown.js`,
    `contextManager`) — sealing a streamed open fence now replicates
    the opener's leading prefix (list indentation, blockquote `>`
    markers), fence char, and run length instead of always appending
    a flush-left ` ``` `. A flush-left closer under a list-indented
    fence is not a closer per CommonMark: it terminates the list and
    opens a NEW top-level fence, which rendered as a persistent
    phantom empty code-block container under the streaming one,
    vanishing with a layout snap when the real closer arrived. Same
    shape for blockquote-nested fences; `~~~` fences were sealed with
    a mismatched ` ``` `. The close toggle is now char/length-aware
    (a bare ``` inside a ```` fence is content, matching marked), and
    the seal drops every trailing run shorter than the opener, not only
    one/two backticks, so longer fences do not grow-then-shrink by a line
    while their closer streams. Streaming volatile-tail only.
    Upstream-PR candidate.
    Regression: `AssistantMessage.test.ts` (fence-seal battery).
12. **incremental list/table/open-fence lexing** (`marked/index.js`,
    `Block.svelte`, `index.js` exports) — the volatile tail re-lexed
    from scratch on every reveal tick, and the boundary splitter
    cannot commit inside a list, table, or open fenced block, so each tick
    cost O(whole block): ~27ms of lexing at 120KB, visible as the "5fps"
    drain the spring couldn't hide. For lists and tables, both parser
    layers reuse the same unit: a completed list item or table source row.
    Append-only growth can touch only the last one. *Block layer:*
    `incrementalLex` + a per-`Block` instance
    `createIncrementalLexCache`; when the block grew append-only and
    the cached tokens are one list (or one `[thead, tbody]` table),
    it re-lexes only from the last item's offset (tables: replays
    the header bytes over the volatile rows) and splices the fresh
    tail onto **reference-identical** sealed tokens, so Svelte's
    `===` prop equality skips their subtrees entirely. An open fenced
    code token instead tracks only the active line's closer-candidate
    state and appends raw/text directly. It bypasses both the whole-source
    incomplete-markdown scan and marked until a valid closer arrives.
    The app code host consumes that token append proof to materialize only
    newly completed lines and to extend its FNV content identity over the
    suffix. It does not reslice or rehash the growing fence body. Its
    `data-code-source=""` attribute is a source-free marker; the rendered
    `<code>` text and Copy button remain the source owners instead of retaining
    a duplicate full-source DOM string.
    *parseBlocks layer:* one cache-owned block array is updated in place. A
    generation-bound append proof lets ordinary paragraphs, one-line headings,
    blockquotes, list items, table rows, and open fences extend their raw
    boundary directly. Other tails re-lex only the last rendered block, or a
    two-block suffix while a marker can still merge backward. A
    `trailingBlock` descent record (kind `list` | `table` | `fence`) lets that
    suffix start inside a large trailing construct. The block-only pass shares
    list/table boundary scanners with the full render lexer, but omits child
    tokens, cell arrays, inline queues, and render metadata it never reads.
    The table scanner maps normalized CRLF matches back to exact source
    offsets, which fixes direct `blockTokens` calls that bypass marked's outer
    newline normalization. Completed compact-render blocks are copied once
    into independent strings; otherwise marked's sliced token raws retain one
    whole historical parser input per completed block.
    Every guard falls back to a full lex. Append lineage is carried in
    opaque generation-bound proofs from the app's formatter/splitter to
    Streamdown and each Block, so stale or fabricated deltas cannot enter
    a fast path. `cache.lastPath` is the test breadcrumb.
    List looseness is monotonic under append-only growth: a
    tight→loose flip takes the full-lex fallback; an already-loose
    list gets its tail items loosened exactly as marked's
    `finalizeList` would. Table bails: footer-marker rows, rowspan
    carets in the volatile slice, AND a caret-shaped cell at the
    cached document's end (a half-arrived `[^t]` momentarily reads
    as a rowspan mark, whose mutation of the previous row the next
    characters revoke — sealed rows are untrustworthy until it
    resolves). Reference-link/footnote definitions are exact, not
    divergent: a definition DECLARED in the tail bails to full
    re-lexes while its line streams (marked's first-wins table, and
    the footnote extension's mutate-the-ref mechanism, cannot
    survive a still-growing definition), and sealed definitions
    carry into tail re-lexes by seeding the merge lexer's links
    table and footnote maps. Cross-block definitions never resolved
    mid-stream upstream (per-block lexing) — unchanged. Blockquotes
    deliberately keep the full-lex fallback: an exact seal would
    replicate marked's per-line strip + lazy-continuation rules, and
    agent prose doesn't produce blockquotes at sizes where it
    matters. Regression: `markdown/incrementalLex.test.ts`
    (byte-equivalence corpus × chunkings, randomized fence differential
    tests, transition tests, and perf contracts for all shapes/layers),
    `markdown/parseBlocksDifferential.test.ts` (fresh-parse and full-render
    parity across deterministic mixed-Markdown streams, source-only
    list/table geometry, and CRLF), `markdown/parseBlocksWorkload.test.ts`
    (real active-pane path and byte budgets),
    `markdown/parseBlockRetention.test.ts` (V8 backing-store tripwire), and
    `chat/streamingIncrementalReuse.browser.test.ts` (DOM identity
    of sealed `<li>`s and `<tr>`s through a real timeline drain).
    The append lexer also carries completion-mode trim bounds from the opaque
    append proof. It slices only the new suffix instead of flattening the full
    active block through `String#trim`. Table caches carry the stable header and
    prior volatile source row. A table append parses only those small strings.
    List and table merges compare tokens only inside the replaced item or row
    because every sealed subtree was already retained by reference. The prior
    recursive comparison walked all 660 sealed rows after each append. A CPU
    profile attributed 79% of the table benchmark to that redundant walk.
    The 660-row steady-append benchmark dropped from about 90ms to 5.5ms per
    100 appends. The same tests cover whitespace trim transitions, provisional
    rowspan carets, partial links, footnotes, and footer fallbacks.
    Agent Overflow's `ChatMarkdown` already isolates the volatile tail at its
    last stable block boundary. It marks that host-proven input with
    `isolatedVolatileTail`. `Streamdown.svelte` skips the document-level
    `parseBlocks` pass only after that parser proved one outer block and left
    an append-stability record for a paragraph, heading/blockquote, list, or
    an open fence whose current line can no longer become a closer. The append
    proof must stay on that physical line. A newline, custom block extension,
    partial fence closer, table, or multi-block result returns to the original
    path. A one-line source alone is not proof: the simple-table grammar can
    split a partial row after a pipe. These guards preserve per-block trimming
    and definition/extension scope while avoiding the duplicate lex before the
    one rendering `Block`.
    The opt-in keeps ordinary Streamdown callers on upstream's document-split
    semantics. The committed instance keeps `parseBlocks` for compact block
    identity and append-only static reconciliation. That cache is idempotent
    for a repeated source and keys only on custom extensions that participate
    in block parsing. Svelte can re-evaluate the committed instance while only
    its volatile sibling changes, and path-link updates replace an inline-only
    extension. Neither event can change an outer block boundary. Treating either
    as invalidation previously full-parsed the growing committed document on
    nearly every reveal tick. Inline token output remains owned by each
    `Block`'s separate `incrementalLex` cache, which still keys on the complete
    extension array. An unchanged parse also republishes the prior state wrapper,
    stopping before `CompactBlocks` instead of making it rescan every committed
    record for a volatile-sibling update. Regression:
    `markdown/streamdownSingleVolatileBlock.test.ts` plus the streamed
    differential and sustained-workload suites listed above.
13. **marker-line alignment isn't code** (`marked/marked-list.js`,
    `tokenizeListItemContent`; `marked/index.js` calls it from
    `loosenTailItems`) — CommonMark reads five or more columns between a
    list marker and its content as "list item starting with indented
    code", so `-     $499 per month` is spec-correctly a code block.
    LLM prose uses that spacing to ALIGN values, never to open a code
    block, and the mis-render is a full code card per bullet. Deliberate
    spec deviation, same scoping as t3-code's remark fix (54f167e2e),
    widened to include the exactly-five-column form t3 leaves as code:
    when a list item's FIRST child token is indented-style `code` (so it
    opened on the marker's own line), the item is re-lexed with the
    marker line's leading run stripped from every line, clamped per
    line. The gate tests the TOKEN, not the raw indentation — block
    extensions tokenize ahead of marked's built-ins, so an aligned
    `$$…$$` is math and must not be dedented out from under the math
    extension. Untouched: `- item\n\n        deep indent` (code, but not
    the first child), `-\n\n    code` (blank marker line closes the
    item; the code is the list's sibling), and any fence
    (`codeBlockStyle` is only `'indented'` for the indented form).
    `tokenizeListItemContent` is the one chokepoint for tokenizing item
    content — `finalizeList`'s tight and loose passes and entry 12's
    `loosenTailItems` all route through it, so a streamed render can't
    disagree with its settled one. Upstream-PR candidate only as an
    opt-in: it is a knowing spec deviation, not a marked bug.
    Regression: `markdown/listMarkerCode.test.ts` (artifact shapes,
    the deliberate-code shapes that must survive, and streamed-vs-settled
    parity across chunkings with the list-append fast path engaged).
14. **blockquote-scoped lexer** (`marked/marked-alert.js`) — the alert
    extension owns EVERY blockquote (it is registered ahead of marked's
    built-ins and returns a plain `blockquote` token when no `[!NOTE]`
    marker is present), and upstream lexed each quoted body through a
    MODULE-LEVEL `Lexer`. `Tokenizer.blockquote` pushes onto
    `this.lexer.inlineQueue`, which only `Lexer.lex()` drains and nothing
    ever called on the singleton — one entry per quoted paragraph,
    retained for the life of the page (16,511 over one test corpus) —
    and its `tokens.links` / footnote maps carried reference definitions
    from one document into the next. Now a blockquote-scoped Lexer,
    thrown away with the call, so there is no field left to remember to
    reset when marked grows another one; a cheap `^ {0,3}>` pre-gate
    keeps the allocation off the block positions that cannot be
    blockquotes at all. Upstream bug, not a deviation — upstream-PR
    candidate. Found by the freeze-replay sweep (now
    `markdown/freezeReplay.manual.ts`, an operator-run driver); the property
    is pinned permanently by `markdown/alertBlockquote.test.ts`, which
    asserts no lexer is reached through twice and that nothing is retained
    between documents.
15. **blockquote `raw` is the consumed prefix** (`marked/marked-alert.js`,
    `consumedPrefix`) — marked rebuilds a blockquote's `raw` by
    re-joining the lines it walked, and its nested-list /
    nested-blockquote continuation branches splice the INNER
    (marker-stripped) token's raw back into the OUTER one. So `raw` came
    back holding bytes the source never had at that offset
    (`">  - - \n$$\n"` → `">  - -\n\n\n$$"`) and sometimes longer than
    the source itself (`"> - a\n[^a]:"` → `"> - a\n[^a]:\n"`). Both
    terminate, which is why it went unnoticed: marked only ever reads
    `raw.length` to advance, and the over-run is one byte past the end of
    what the block rule matched, so nothing crashed and no content was
    swallowed. What it broke is the contract this whole pipeline's
    incremental machinery is built on — a block token's `raw` names the
    bytes it consumed. That is what `parseBlocks`' contiguity sum and
    `incrementalLex`'s raw-offset arithmetic (entry 12) test their
    offsets against; a longer raw makes the sum unusable, and a
    same-length-different-bytes raw passes it while the cached block's
    raw no longer describes the source. `markedAlert` is registered in
    the lex pass and not the block pass, so today the exposure is via
    `lex()` / `incrementalLex` — which is exactly why it must be fixed
    before the next thing starts summing raws. The token now reports the
    prefix of `src` that `raw.length` names (consumption is unchanged),
    clamped to the block rule's own match minus the newlines the
    tokenizer rtrims. Upstream bug in marked itself, corrected at our
    boundary — `Tokenizer.blockquote` cannot be fixed from here.
    Regression: `markdown/alertBlockquote.test.ts` (both found shapes,
    plus the well-formed blockquotes that must stay byte-identical).
16. **the fullscreen rule is scoped to mermaid** (`Elements/Mermaid.svelte`)
    — upstream's expand style is `:global([data-expanded='true'])`, with no
    scope at all. `:global` means app-wide, and `data-expanded` is an
    ordinary attribute name, so ANY element anywhere in the app that
    carried `data-expanded="true"` was pulled out of its layout and pinned
    `position: fixed` over the whole viewport at `z-index: 2147483647`. The
    workflows run map's expanded wave row hit it: the wave rendered on top
    of the entire app and its 1248×688 box swallowed every click meant for
    the run detail's action row underneath. A stylesheet from a markdown
    renderer must not be able to reach a component that has never rendered
    markdown. Now `:global([data-streamdown-mermaid] [data-expanded='true'])`
    — the element upstream meant, and only it; the container carrying the
    attribute is always a child of that wrapper, so mermaid's own behaviour
    is unchanged. Upstream bug, upstream-PR candidate. Note that this app
    never reaches mermaid's fullscreen path anyway: `StreamdownMermaidHost`
    intercepts "Toggle expand" in the capture phase and routes it to
    `DiagramModal` (see `markdown/mermaidExpandIntercept.test.ts`), because
    the library's overlay lands off-screen inside the virtualizer's
    containment-scoped rows. The run map has since moved off the attribute
    as well (`data-wave-expanded`): the scoping above is the fix, and a
    component staying out of a vendored stylesheet's namespace is the belt
    to its braces — neither alone would have been enough to trust, since
    `data-expanded` is exactly the name the next component will reach for.
    Regression: `e2e/tests/workflows-overlay.spec.ts` clicks the action row
    through a map with an expanded wave — which is the shape that failed.
17. **path-relative hrefs never render raw anchors** (`Elements/Link.svelte`)
    — upstream renders any `/`-leading href (`isPathRelativeUrl`, literally
    `startsWith('/')`) through a branch that BYPASSES `transformUrl` and
    emits the raw href with no `target`/`rel`. In an SPA host that anchor
    is a same-tab top-level navigation onto the app origin: for a chat
    message carrying `[x](/home/user/file.md)` that's a 404 against the
    transport server; for a crafted `[x](/design/...)` it was a confirmed
    origin-isolation escape (agent-authored HTML served same-origin could
    read the bootstrap token — `docs/specs/remote-access-boundaries.md`
    §Confirmed defects), and `//host/x` counted as "relative" too, giving
    protocol-relative navigation off-origin. The branch is removed: an
    anchor renders only for a `transformUrl`-approved href (which includes
    the app's nonce'd `agent-overflow:open` scheme), and everything else
    falls to the non-navigable reference span from entry 10. Hosts that
    want path-shaped hrefs to DO something rewrite them during parsing —
    the app's `pathLinkExtension.ts` turns them into editor path links.
    Deliberate deviation for this host, not an upstream-PR candidate as-is
    (upstream's browser-page use case may genuinely want relative
    navigation; an upstream fix would be opt-in). Regression:
    `ChatMarkdown.test.ts` ("never renders a raw same-origin anchor…").
    Same fix in `Elements/Image.svelte` (2026-08-18 follow-up): upstream's
    image element had the identical `isPathRelativeUrl` bypass on `src` —
    `![x](/anything)` issued a raw same-origin GET against the transport
    server and `![x](//host/x.png)` a protocol-relative off-origin fetch,
    both without consulting `transformUrl`. An `<img>` now renders only
    for a `transformUrl`-approved src; path-relative image srcs fall to
    the blocked-image span (no AO surface produces them — chat
    attachments render through dedicated components, not markdown).
    Regression: `ChatMarkdown.test.ts` ("never renders a raw
    same-origin img…").
18. **the mermaid SVG cache is keyed on the PALETTE, not on `theme`**
    (`Elements/Mermaid.svelte`) — entry 9's cache keyed on
    `(mermaidConfig.theme, sanitized source)`, with an explicit note that
    other config fields were the caller's problem. That holds only while
    `theme` NAMES the palette. A host that drives mermaid from its own
    design tokens pins `theme: 'base'` permanently — it is the one
    mermaid theme that derives every color from `themeVariables` — and
    varies `themeVariables` instead, so light and dark collapse onto the
    same `base:<source>` key: the first diagram rendered after boot wins
    for the rest of the page, and a theme flip re-serves the old colors
    under a remount that exists precisely to repaint them. The key now
    carries a `JSON.stringify` of `themeVariables`' sorted entries
    alongside `theme` — stringified rather than joined because the values
    are font stacks and color functions full of commas, and a `k=v` join
    is delimiter-ambiguous (`{a: 'x,b=y'}` and `{a: 'x', b: 'y'}` collide,
    serving one palette's SVG for another). Cheap by construction (a flat
    object of ~12 entries, rebuilt only when the palette changes), and it
    gives the SAME PARTITIONING as upstream's key when no `themeVariables`
    are passed, where the palette segment is empty; the exact
    serialization differs by a constant separator, which is irrelevant to
    a per-page cache that nothing outside this file reads.
    Fields that are neither `theme` nor `themeVariables`
    (flowchart curve, `securityLevel`, …) stay out of the key, exactly as
    entry 9 left them. The app-side producer of the variables is
    `components/chat/markdown/mermaidTokens.ts`, handed down as
    `ChatMarkdown`'s `mermaidConfig` prop. Upstream bug once
    `themeVariables` are in play, upstream-PR candidate. **Drop rule:**
    if upstream ever folds `themeVariables` (or any whole-config hash)
    into its own cache key, delete this hunk and take upstream's — the
    regression below asserts the observable property, not our exact
    serialization. Regression: `markdown/mermaidCacheKey.test.ts`.
19. **allowed links render their Markdown title** (`Elements/Link.svelte`)
    — upstream passes `token.title` only to a custom link snippet. Its
    default anchor drops the title, so standard Markdown link titles and
    host-generated action tooltips disappear unless every host replaces the
    whole link renderer. The default anchor now emits the title attribute
    when the token has one. Upstream bug, upstream-PR candidate. Regression:
    `ChatMarkdown.test.ts` (editor-link hover text).
20. **theme and mermaidConfig resolution memoized** (`Streamdown.svelte`)
    — upstream's context getters rebuilt their objects on every access:
    `get theme()` ran `mergeTheme` (a deep merge invoking
    `twMerge(clsx(...))` per subkey across the whole theme) and
    `get mermaidConfig()` spread a fresh object, per read. Every template
    effect of every element component reads `streamdown.theme.*`, so
    sustained streaming re-ran the full merge thousands of times a
    second — profiled at 33MB/45s of allocation on the soak burn rig,
    plus a fresh object identity per read that defeated every downstream
    equality check. Both are now `$derived` in the component body
    (`resolvedTheme`, `resolvedMermaidConfig`), so the merge happens
    once per (theme, baseTheme) / (mermaidConfig, darkMode) change and
    the getters serve cached objects. No behavioral change: no consumer
    could have relied on per-read identity (it was never stable), and
    reactivity is preserved because the deriveds track the same props
    the getters read. Upstream perf bug, upstream-PR candidate.
    Regression: `markdown/streamdownThemeMemo.test.ts` (identity stable
    across reads, re-mints on theme prop change).
21. **the active trailing literal leaf has ONE imperative owner**
    (`Streamdown.svelte`, `Block.svelte`, `LiteralHost.svelte`,
    `literal-host.ts`). A streaming prose update used
    to replace the whole assistant row and run Svelte, block splitting,
    marked, style, and layout for every revealed word. The active volatile
    block now marks only its final text leaf when every token on that path is
    a literal text container, including the table section/row/cell chain.
    Agent Overflow can append safe word deltas in bounded sibling Text nodes
    and reserve an authoritative render for punctuation or structural
    markdown. Table pipes, row breaks, alignment markers, and rowspan carets
    remain structural deltas. Links and unknown extension tokens are excluded.
    The marker does not alter settled output and `display:
    contents` adds no layout box.

    **The marked leaf is rendered by a controller, never by Svelte.**
    `LiteralHost.svelte` renders an EMPTY span and attaches
    `literal-host.ts`'s controller, which is the single writer of that
    element's children; the renderer publishes the parser's authoritative
    leaf text INTO it (`publish(token, text)`) instead of rendering a Text
    node of its own. An app-side owner may `adopt` the element and take over
    presentation while it streams, and an authoritative update is then routed
    through that owner rather than written behind its back. Adoption mutates
    nothing — the owner inherits exactly what is on screen. Token identity is
    half the update proof: equal text under a NEW token is a structural change
    that still reconciles, and the same token with the same text is an
    unrelated rerender that must not disturb a live revealed suffix.

    This replaces the previous split ownership, and the previous claim that
    app code "owns all appended nodes and removes them before fallback" is
    now FALSE by design. Two writers on one visible text run was a defect,
    not a division of labour: the app deleted its appended siblings in one
    task and Svelte re-extended its own parser-owned node in a later one, so
    the visible string shrank back to an older parser checkpoint and regrew.
    A `MutationObserver` repro caught three such rollbacks in one paragraph
    reveal (a long prefix dropping back to `The hour-long`). Chromium
    coalesces that before the next frame; WebView2 is allowed to present it,
    which is what the user saw as a settle flicker and an extra line of text
    at completion. The rule both writers are now held to is ownership, not
    timing: **the visible string only ever EXTENDS, until a genuine
    divergence replaces it in a single mutation record.** A fallback
    relinquishes the RUN and never deletes visible bytes; `replaceChildren`
    is the divergence primitive because the DOM `replace all` algorithm
    queues ONE record carrying both the removals and the addition, so no
    reader can observe between the two halves.

    The pane updates the raw canonical row in place while
    every mounted assistant-body projection receives the same suffix. This
    keeps imperative reads and later mounts current without waking Streamdown.
    App-side ownership relinquishes that checkpoint on source, metadata,
    workspace, view-only, streaming-mode, and volatile-tail changes, and
    restores its literal suffix if another visible projection remounts. The
    owner rejects
    incomplete-parser leaves whose text contains a synthetic trailing byte,
    and path-link completion scans only a bounded crossing tail. Host-specific
    performance extension, not an upstream bug. Drop it if upstream exposes an
    equivalent incremental trailing-leaf hook — an upstream hook must offer
    the same single-owner guarantee, not merely an append point. Regression:
    `ChatMarkdown.directRevealDifferential.test.ts`,
    `ChatMarkdown.directRevealMonotonic.browser.test.ts` (mutation-level
    monotonicity: the rollback class, red against a reintroduced
    delete-before-write),
    `ChatMarkdown.directRevealSelection.browser.test.ts`, and
    `AssistantMessage.streamingReveal.test.ts`, plus
    `markdown/streamingAssistantLiteralOwner.test.ts` and
    `stores/streamingAssistantReveal.test.ts` for ownership transitions.
22. **default completed blocks use one compact fixed-tag owner**
    (`Streamdown.svelte`, `CompactBlocks.svelte`, `document-interaction.ts`,
    `Block.svelte`, `static-html.ts`). Upstream sends each marked token through a component, a
    full token-type branch, a `Slot` boundary, and recursive snippets. Svelte
    retains the resulting control-flow anchors, reactions, closures, and empty
    Text placeholders after the answer settles. A 7.3K-character rich answer
    held more than 10K such non-element nodes in Blink, multiplied across every
    visible pane. The completed Streamdown instance now mounts one
    `CompactBlocks` owner over the parser cache's stable block array. It keeps
    reference-identical prefix DOM, appends fixed-tag HTML for new blocks, and
    rebuilds only a rewritten suffix. It mounts a real `Block` island when
    serialization rejects a custom or component-backed token, and explicitly
    unmounts that island when the suffix changes or the owner dies.
    Model-authored text, attributes, and URLs pass through the same escaping
    and URL transformation rules. A host can provide a synchronous
    `staticRenderers` callback for a completed custom code component. A cache
    miss mounts the real component so its async work remains authoritative.
    A component publishes a per-record retry when its synchronous renderer is
    ready, even if an unrelated live code request still holds Streamdown's
    global async settle count. The global gate continues to own `onsettled`,
    but it cannot retain every older code component for a whole long turn.
    Retirement is coordinated across Streamdown owners and processes at most
    one component record per owner per frame, bounding a simultaneous settle
    without blocking ready siblings. One shared document-event
    snapshot tracks selection ranges and focus. This keeps Blink's live
    `Selection.isCollapsed` read, which flushes pending whole-message style,
    out of every island retirement. Selection/focus start and clear transitions
    both schedule a retry, so interaction postpones replacement without
    stranding the island afterward. Volatile blocks keep reactive components, but
    common default tokens render directly in `Block` instead of crossing the
    generic `Element` component. This preserves streaming updates, extension
    behavior, selection, copy text, and final DOM semantics while removing the
    settled control-flow tree. Upstream performance direction with a
    host-specific completed-block fast path. Regression:
    `ChatMarkdown.compactStaticDifferential.test.ts` checks compact versus full
    semantic DOM and input escaping, `ChatMarkdown.domBudget.test.ts` enforces
    the control-node budget, `ChatMarkdown.compactStaticAppend.test.ts` covers
    append/rewrite/island lifecycle and selection identity,
    `ChatMarkdown.codeSpans.test.ts` (including sibling-pending and coordinated
    frame scheduling) and
    `ChatMarkdown.codeSpans.browser.test.ts` cover async code-island
    retirement plus focus/selection transitions, and
    `ChatMarkdown.directRevealSelection.browser.test.ts` covers selection
    identity while the live tail changes.
23. **backslash escapes render their literal character**
    (`Elements/Element.svelte`, `static-html.ts`). Marked emits an `escape`
    token for forms such as `\*`, but upstream's element branch emitted only a
    TODO comment. Escaped punctuation therefore vanished from both settled and
    streaming output. Both render paths now emit the token's text value.
    Upstream bug, upstream-PR candidate. Regression: `ChatMarkdown.test.ts`
    (backslash-escaped punctuation).
24. **extension tokenizers reject impossible starts before calling Svelte or
    regex grammars** (`marked/marked-{footnotes,br,citations,math,subsup}.js`).
    Marked invokes every registered tokenizer at each candidate position.
    The inline footnote tokenizer called `getContext` before checking for a
    `[^` opener. Parser work runs outside component initialization, so ordinary
    prose paid a thrown and caught Svelte lifecycle error on every inline token.
    The other tokenizers ran anchored regexes when the first code unit could
    not match. Character-code gates now return before either cost. This changes
    no accepted source. The extension-heavy 20,000-parse benchmark fell from
    1.98s to 0.46s after this fix and the Marked dispatcher patch documented in
    `frontend/AGENTS.md`. Regression:
    `markdown/parseBlocksDifferential.test.ts` (zero grammar calls for
    impossible extension starts), plus the full incremental differential
    corpus.
25. **the rendered root publishes its final block type** (`Streamdown.svelte`).
    The host needs the committed-to-volatile seam to match ordinary Markdown
    spacing only when the committed prefix ends in a paragraph. A CSS `:has()`
    query over the committed subtree answered that question, but every streamed
    descendant insertion invalidated the ancestor selector and widened style
    recalculation across all visible panes. Streamdown now derives the last
    rendered token type from its existing block array and exposes it as
    `data-streamdown-last-block`. The attribute changes only when the last block
    type changes. It does not alter markup semantics. Host-specific performance
    metadata, not an upstream bug. Drop it if the host no longer splits one
    document across adjacent Streamdown roots. Regression:
    `ChatMarkdown.boundarySpacing.test.ts` and
    `ChatMarkdown.boundarySpacing.browser.test.ts`.
26. **message-edge margin trimming uses explicit block markers**
    (`Streamdown.svelte`, `context.svelte.d.ts`). Chat rows remove the first
    block's top margin and the last block's bottom margin. Structural
    `:first-child` / `:last-child` selectors made every nested sibling change,
    including highlighted syntax-span replacement, schedule an invalidation
    set over all prior `md-blk` elements. A 65K-character four-pane run reached
    roughly 4,600 old elements and 32ms per frame-time style pass. The host now
    states whether each Streamdown root owns the message's first or last edge.
    Streamdown marks only the corresponding direct block with `sd-trim-*` and
    moves that class when the edge block changes. Text and syntax descendants
    cannot trigger the edge rule. The marker changes no margin or markup
    semantics. It replaces the exact structural selector that owned those
    margins. Host-specific performance metadata, not an upstream bug.
    Regression coverage is `ChatMarkdown.boundarySpacing.test.ts`,
    `rowMarginContainment.browser.test.ts`, and `styleInvalidation.test.ts`.
27. **host styling predicates use parser-owned classes**
    (`Streamdown.svelte`, `Block.svelte`, `Elements/Element.svelte`,
    `CompactBlocks.svelte`, `static-html.ts`, `paragraph-spacing.ts`). Three
    ordinary Markdown rules had document-wide invalidation keys. The volatile
    seam used `p:first-child`, task items used `li:has(> input)`, and settled
    paragraph spacing used `p + p`. Replacing a completed code island then
    matched every old paragraph and list item in every visible pane. Streamdown
    now marks the direct first block, task-list renderers mark the owning item,
    and completed renderers derive paragraph adjacency after nodes enter their
    final parent. `CompactBlocks` updates only newly appended or replaced
    nodes. The non-compact completed path scans its root once. Volatile
    paragraphs retain a class-keyed sibling rule because that tree contains
    only the bounded unstable tail. These classes reproduce the same selector
    predicates without making unrelated descendant changes candidates. A
    five-minute four-pane trace kept the largest steady style pass to 112
    elements after the change. Host-specific performance metadata, not an
    upstream bug. Regression coverage is
    `ChatMarkdown.boundarySpacing.test.ts`,
    `ChatMarkdown.boundarySpacing.browser.test.ts`,
    `ChatMarkdown.compactStaticAppend.test.ts`, and
    `styleInvalidation.test.ts`.
