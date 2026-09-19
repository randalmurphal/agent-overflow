<script lang="ts">
  // The expanded body of an AO tool row (chat/aoTools.ts): what the header
  // could not show, then the result. The header keeps one clipped line, so
  // the full text goes first when it was clipped; the inputs the header
  // left out follow, each named for a person (the computer, the job, the
  // page) rather than by id; and a remote reply renders as its outcome and
  // output instead of the JSON the model read.
  import type { PayloadExpansionHandle } from '../../utils/payloadExpansion.svelte';
  import type {
    PaneSession,
    RowUiRegistry,
    ScrollHost,
  } from '../../stores/threadPaneRoles';
  import { nestedScroll } from '../../utils/scroll/wheelAttribution';
  import AnsiText from './AnsiText.svelte';
  import ExpandablePayloadBody from './ExpandablePayloadBody.svelte';
  import {
    REMOTE_TOOLS_SERVER,
    THREAD_TOOLS_SERVER,
    remoteResultView,
    threadResultView,
    type AoToolFact,
    type AoToolPresentation,
  } from './aoTools';

  let {
    pane,
    expansion,
    id,
    tool,
    facts,
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
    hasPayload: boolean;
    emptyMessage: string;
    deferredOutputState?: string;
    deferredOutputError?: string;
  } = $props();

  let clipped = $derived(tool.fullWhat !== tool.what);
  let hasDetails = $derived(clipped || facts.length > 0);
</script>

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
      {#each facts as fact (fact.label)}
        <div class="flex gap-2">
          <dt class="shrink-0 text-fg-hint">{fact.label}</dt>
          <dd class="min-w-0 whitespace-pre-wrap break-all" data-testid="tool-call-card-ao-fact" data-fact={fact.label}>{fact.value}</dd>
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
  >
    {#snippet renderContent({ data, testId })}
      {@const view = tool.server === REMOTE_TOOLS_SERVER ? remoteResultView(data) : null}
      {@const thread = tool.server === THREAD_TOOLS_SERVER ? threadResultView(data) : null}
      <div
        class="ansi-body min-w-0 max-w-full max-h-60 overflow-y-auto overflow-x-hidden whitespace-pre-wrap break-words px-3 py-2 text-[0.6875rem] leading-relaxed text-fg-muted"
        use:nestedScroll
        data-testid={testId}
      >
        {#if thread}
          <p class="mb-1 text-fg-hint" data-testid="tool-call-card-thread-title">
            {thread.title || thread.summary}{thread.state ? ` · ${thread.state}` : ''}
          </p>
          <AnsiText source={data} class="whitespace-pre-wrap break-all" />
        {:else if view}
          <p class="mb-1 text-fg-hint" data-testid="tool-call-card-remote-outcome">{view.outcome}</p>
          {#if view.error}
            <p class="mb-1 break-words text-error">{view.error}</p>
          {/if}
          {#if view.head}
            <AnsiText source={view.head} class="whitespace-pre-wrap break-all font-mono" />
            <p class="my-1 text-fg-hint">…</p>
          {/if}
          {#if view.output}
            <AnsiText source={view.output} class="whitespace-pre-wrap break-all font-mono" />
          {:else if !view.head}
            <p class="text-fg-hint">No output.</p>
          {/if}
          {#if view.hint}
            <p class="mt-1 text-fg-hint">{view.hint}</p>
          {/if}
        {:else}
          <AnsiText source={data} class="whitespace-pre-wrap break-all" />
        {/if}
      </div>
    {/snippet}
  </ExpandablePayloadBody>
{/if}
