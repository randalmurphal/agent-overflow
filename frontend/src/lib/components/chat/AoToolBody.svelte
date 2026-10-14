<script lang="ts">
  // The expanded body of an AO tool row (chat/aoTools.ts): what the header
  // could not show, then the result. The header keeps one clipped line, so
  // the full text goes first when it was clipped; the inputs the header
  // left out follow, each named for a person (the computer, the job, the
  // page, the thread, the run) rather than by id; then the reply as its
  // server's decoder reads it (chat/aoToolResults.ts): the outcome, a link
  // to what the call touched, its facts, one line per listed entity and
  // long text folded behind "show more". A failed call shows its message.
  // A reply past the payload preview is read whole before it is decoded,
  // and text no decoder reads is reported, not painted, until "Show raw".
  // Nothing here renders the JSON the model read by default.
  import type { PayloadExpansionHandle } from '../../utils/payloadExpansion.svelte';
  import type {
    PaneSession,
    RowUiRegistry,
    ScrollHost,
  } from '../../stores/threadPaneRoles';
  import type { BackendKey } from '../../transport/backendKey';
  import { formatPayloadSize } from '../../utils/payloadExpansion.svelte';
  import { nestedScroll } from '../../utils/scroll/wheelAttribution';
  import AnsiText from './AnsiText.svelte';
  import AoFoldedText from './AoFoldedText.svelte';
  import AoToolLink from './AoToolLink.svelte';
  import ExpandablePayloadBody from './ExpandablePayloadBody.svelte';
  import RowError from './RowError.svelte';
  import { preservePaneScrollAnchor } from './preserveScrollAnchor';
  import {
    FACT_MAX,
    REMOTE_TOOLS_SERVER,
    type AoToolFact,
    type AoToolNames,
    type AoToolPresentation,
  } from './aoTools';
  import {
    LIST_LINES,
    PREVIEW_LINES,
    PREVIEW_MAX,
    aoResultView,
    aoToolError,
    remoteResultView,
  } from './aoToolResults';

  let {
    pane,
    expansion,
    id,
    tool,
    facts,
    input,
    names,
    viewer,
    failed = false,
    live = '',
    answered = false,
    errorShown = '',
    hasPayload,
    emptyMessage,
    deferredOutputState = '',
    deferredOutputError = '',
  }: {
    pane?: PaneSession & RowUiRegistry & ScrollHost;
    expansion: PayloadExpansionHandle;
    id: string;
    tool: AoToolPresentation;
    facts: AoToolFact[];
    input: Record<string, unknown>;
    names: AoToolNames;
    viewer: BackendKey;
    /** The call failed: the reply is an error message, not a result. */
    failed?: boolean;
    /** Where a request stands now, when the row follows one ("answer arrived"). */
    live?: string;
    /** The request's answer has landed, so the reply's "still working" note no longer applies. */
    answered?: boolean;
    /** The error message the row's header already shows; the body does not repeat it. */
    errorShown?: string;
    hasPayload: boolean;
    emptyMessage: string;
    deferredOutputState?: string;
    deferredOutputError?: string;
  } = $props();

  // A fact value folds once it runs past a few lines.
  const FACT_LINES = 4;

  // The decoders read a reply whole, and the expansion fetches its first
  // 32 KB: a reply past that loads the rest before it is decoded, up to
  // this size. A thread_show or thread_item read is at most 1 MiB of text.
  const AO_REPLY_MAX = 2 * 1024 * 1024;

  let clipped = $derived(tool.fullWhat !== tool.what);
  let hasDetails = $derived(clipped || facts.length > 0);
  let allLines = $state(false);
  let rawShown = $state(false);
  let partial = $derived(expansion.displayData !== null && !expansion.isComplete);
  let tooLarge = $derived(partial && expansion.totalSize > AO_REPLY_MAX);

  $effect(() => {
    if (!expansion.expanded || !partial || tooLarge) return;
    if (expansion.loading || expansion.error !== null) return;
    void expansion.showFull();
  });

  function toggleRaw(event: MouseEvent): void {
    event.stopPropagation();
    preservePaneScrollAnchor(pane, event, () => {
      rawShown = !rawShown;
    });
  }
</script>

{#snippet rawToggle()}
  <button
    type="button"
    class="text-accent hover:underline focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50 rounded compact:select-none"
    aria-expanded={rawShown}
    data-testid="tool-call-card-ao-raw-toggle"
    onclick={toggleRaw}
  >{rawShown ? 'Hide raw' : 'Show raw'}</button>
{/snippet}

{#if hasDetails}
  <div
    {id}
    class="ml-5 border-l border-border-subtle bg-surface-0/35"
    data-testid="tool-call-card-ao-body"
  >
    <dl class="max-h-60 overflow-y-auto px-3 py-2 text-[0.6875rem] leading-relaxed text-fg-muted" use:nestedScroll>
      {#if clipped}
        <div class="mb-1">
          <dt class="text-fg-hint">{tool.label}</dt>
          <dd class="whitespace-pre-wrap break-all font-mono" data-testid="tool-call-card-ao-full">{tool.fullWhat}</dd>
        </div>
      {/if}
      {#each facts as fact, index (index)}
        <div class="flex gap-2">
          <dt class="shrink-0 text-fg-hint">{fact.label}</dt>
          <dd class="min-w-0 break-all" data-testid="tool-call-card-ao-fact" data-fact={fact.label}>
            <AoFoldedText {pane} text={fact.value} lines={FACT_LINES} max={FACT_MAX} testId="tool-call-card-ao-fact-text" />
          </dd>
        </div>
      {/each}
    </dl>
  </div>
{/if}

{#if hasPayload}
  <ExpandablePayloadBody
    {pane}
    {expansion}
    id={hasDetails ? `${id}-result` : id}
    testPrefix="tool-call-card"
    {emptyMessage}
    {deferredOutputState}
    {deferredOutputError}
    copyLabel="Copy result"
    offerMore={tooLarge && rawShown}
  >
    {#snippet renderContent({ data, testId })}
      {@const error = failed && !partial ? aoToolError(data) : null}
      {@const remote = !failed && !partial && tool.server === REMOTE_TOOLS_SERVER ? remoteResultView(data) : null}
      {@const view = failed || remote || partial ? null : aoResultView(tool.server, tool.tool, data, input, names)}
      <div
        class="ansi-body min-w-0 max-w-full max-h-60 overflow-y-auto overflow-x-hidden px-3 py-2 text-[0.6875rem] leading-relaxed text-fg-muted"
        use:nestedScroll
        data-testid={testId}
      >
        {#if partial}
          <p class="mb-1 flex flex-wrap items-baseline gap-x-1.5 text-fg-hint" data-testid="tool-call-card-ao-undecoded">
            {#if tooLarge}
              <span>Reply of {formatPayloadSize(expansion.totalSize)}, too large to read here.</span>
              {@render rawToggle()}
            {:else}
              <span role="status">Reading the whole reply ({formatPayloadSize(expansion.totalSize)})…</span>
            {/if}
          </p>
          {#if rawShown}
            <AnsiText source={data} class="whitespace-pre-wrap break-all font-mono" />
          {/if}
        {:else if error}
          <div data-testid="tool-call-card-ao-error">
            {#if error.message === errorShown}
              <p class="text-fg-hint">The message is shown above.</p>
            {:else}
              <RowError tone="error" msg={error.message} code={error.code || undefined} />
            {/if}
          </div>
        {:else if remote}
          <p class="mb-1 text-fg-hint" data-testid="tool-call-card-remote-outcome">{remote.outcome}</p>
          {#if remote.error}
            <p class="mb-1 break-words text-error">{remote.error}</p>
          {/if}
          {#if remote.head}
            <AnsiText source={remote.head} class="whitespace-pre-wrap break-all font-mono" />
            <p class="my-1 text-fg-hint">…</p>
          {/if}
          {#if remote.output}
            <AnsiText source={remote.output} class="whitespace-pre-wrap break-all font-mono" />
          {:else if !remote.head}
            <p class="text-fg-hint">No output.</p>
          {/if}
          {#if remote.hint}
            <p class="mt-1 text-fg-hint">{remote.hint}</p>
          {/if}
        {:else if view}
          {#if view.outcome || view.link}
            <p class="mb-1 flex min-w-0 flex-wrap items-baseline gap-x-1.5" data-testid="tool-call-card-ao-outcome">
              {#if view.link}
                <AoToolLink link={view.link} {viewer} testId="tool-call-card-ao-link" />
              {/if}
              {#if view.outcome}
                <span class="min-w-0 break-words text-fg-hint">{view.link ? '· ' : ''}{view.outcome}{live ? ` → ${live}` : ''}</span>
              {/if}
            </p>
          {/if}
          {#if view.facts.length > 0}
            <dl class="mb-1">
              {#each view.facts as fact, index (index)}
                <div class="flex gap-2">
                  <dt class="shrink-0 text-fg-hint">{fact.label}</dt>
                  <dd class="min-w-0 break-words" data-testid="tool-call-card-ao-result-fact" data-fact={fact.label}>
                    <AoFoldedText {pane} text={fact.value} lines={FACT_LINES} max={FACT_MAX} testId="tool-call-card-ao-result-fact-text" />
                  </dd>
                </div>
              {/each}
            </dl>
          {/if}
          {#if view.lines.length > 0}
            <ul class="mb-1 space-y-1" data-testid="tool-call-card-ao-lines">
              {#each allLines ? view.lines : view.lines.slice(0, LIST_LINES) as line, index (`${line.key}:${index}`)}
                <li class="min-w-0" data-testid="tool-call-card-ao-line">
                  <div class="flex min-w-0 items-baseline gap-1.5">
                    {#if line.link}
                      <AoToolLink link={line.link} {viewer} testId="tool-call-card-ao-line-link" />
                    {:else}
                      <span class="min-w-0 truncate text-fg-muted">{line.title}</span>
                    {/if}
                    {#if line.detail}
                      <span class="min-w-0 truncate text-fg-hint">· {line.detail}</span>
                    {/if}
                  </div>
                  {#if line.text}
                    <p class="line-clamp-2 whitespace-pre-wrap break-words text-fg-subtle">{line.text}</p>
                  {/if}
                </li>
              {/each}
            </ul>
            {#if view.lines.length > LIST_LINES}
              <button
                type="button"
                class="mb-1 text-accent hover:underline focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50 rounded compact:select-none"
                aria-expanded={allLines}
                data-testid="tool-call-card-ao-lines-toggle"
                onclick={(event) => {
                  event.stopPropagation();
                  preservePaneScrollAnchor(pane, event, () => {
                    allLines = !allLines;
                  });
                }}
              >{allLines ? 'Show fewer' : `Show all ${view.lines.length}`}</button>
            {/if}
          {/if}
          {#if view.preview}
            <div class="mb-1">
              <p class="text-fg-hint">{view.preview.label}</p>
              <AoFoldedText
                {pane}
                text={view.preview.text}
                lines={PREVIEW_LINES}
                max={PREVIEW_MAX}
                mono
                testId="tool-call-card-ao-preview"
              />
            </div>
          {/if}
          {#if view.note && !answered}
            <p class="text-fg-hint" data-testid="tool-call-card-ao-note">{view.note}</p>
          {/if}
        {:else}
          <!-- Text no decoder reads: said, not painted, until asked for. -->
          <p class="mb-1 flex flex-wrap items-baseline gap-x-1.5 text-fg-hint" data-testid="tool-call-card-ao-undecoded">
            <span>Reply not decoded, {formatPayloadSize(new TextEncoder().encode(data).length)}.</span>
            {@render rawToggle()}
          </p>
          {#if rawShown}
            <AnsiText source={data} class="whitespace-pre-wrap break-all font-mono" />
          {/if}
        {/if}
      </div>
    {/snippet}
  </ExpandablePayloadBody>
{/if}
