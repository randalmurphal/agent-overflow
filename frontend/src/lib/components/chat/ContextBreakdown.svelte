<script lang="ts">
  // Claude's canonical `/context` breakdown, read on demand from the live
  // session and rendered inside the context meter's popover.
  //
  // Deliberately NOT a store: the numbers describe the provider process
  // right now, and Core Principle 2 says we don't duplicate provider state.
  // Nothing outlives this component — closing the popover unmounts it and
  // the next open re-reads. There is no polling and no refresh timer; the
  // always-on meter is still driven by streaming usage events.
  import { GetThreadContextUsage } from '../../stores/bindings';
  import { hasScope } from '../../transport/scopes';
  import { formatTokens } from '../../utils/format';

  let { threadId }: { threadId: string } = $props();

  interface Row {
    name: string;
    tokens: number;
    deferred: boolean;
  }

  type State =
    | { kind: 'loading' }
    | { kind: 'unavailable'; reason: string }
    | { kind: 'error'; message: string }
    | {
        kind: 'ready';
        totalTokens: number;
        maxTokens: number;
        percentage: number;
        rows: Row[];
        hasDeferred: boolean;
      };

  let state = $state<State>({ kind: 'loading' });

  // The prop as a $derived primitive. Svelte props are plain getters, so
  // reading `threadId` in the effect subscribes through the parent's
  // expression to `pane.thread` itself — a $state replaced wholesale on
  // every thread-row sync (activity touch, status patch, read-mark). Each
  // replacement re-fired this read — a control round-trip to the live CLI
  // plus a full re-tokenization — while the popover was open, cancelling
  // the in-flight one. The $derived does not propagate while the id is
  // unchanged, so the read runs once per thread as intended.
  const tid = $derived(threadId);

  // One read per thread: the effect tracks the id, so a pane that swaps
  // threads under an open popover cancels the in-flight read and re-reads
  // for the new one rather than showing the old thread's numbers.
  $effect(() => {
    let cancelled = false;
    // The exact read rides `threads:operate`, so a session without it has
    // one possible answer and asking would only spend a refusal to reach
    // it. The popover's own unavailable state says so in place; the meter
    // above keeps rendering the estimate, which is a read.
    if (!hasScope('threads:operate')) {
      state = { kind: 'unavailable', reason: 'The exact breakdown is not available for this session.' };
      return;
    }
    GetThreadContextUsage(tid)
      .then((usage) => {
        if (cancelled) return;
        if (!usage.available) {
          state = {
            kind: 'unavailable',
            reason: usage.reason || 'The exact breakdown is not available right now.',
          };
          return;
        }
        // Zero-token categories carry no information; everything else
        // passes through in the CLI's own order, including names this
        // build has never heard of.
        const rows: Row[] = usage.categories
          .filter((cat) => cat.tokens > 0)
          .map((cat) => ({
            name: cat.name,
            tokens: cat.tokens,
            deferred: cat.deferred === true,
          }));
        state = {
          kind: 'ready',
          totalTokens: usage.totalTokens,
          maxTokens: usage.maxTokens,
          percentage: usage.percentage,
          rows,
          hasDeferred: rows.some((row) => row.deferred),
        };
      })
      .catch((err: unknown) => {
        if (cancelled) return;
        state = { kind: 'error', message: err instanceof Error ? err.message : String(err) };
      });
    // A response that lands after the popover closed must not write into a
    // reopened popover's state — the reopened one is running its own fetch.
    return () => {
      cancelled = true;
    };
  });
</script>

<div class="mt-2 border-t border-border-subtle pt-2">
  {#if state.kind === 'loading'}
    <p class="text-xs text-fg-hint">Reading exact usage…</p>
  {:else if state.kind === 'unavailable'}
    <p class="text-xs text-fg-hint">{state.reason}</p>
  {:else if state.kind === 'error'}
    <p class="text-xs text-error">Couldn't read the breakdown: {state.message}</p>
  {:else}
    <p class="mb-1 text-xs text-fg-muted tabular-nums">
      Exact: {formatTokens(state.totalTokens)} / {formatTokens(state.maxTokens)} ({state.percentage}%)
    </p>
    {#if state.rows.length > 0}
      <ul class="space-y-0.5 text-xs text-fg-muted">
        {#each state.rows as row (row.name)}
          <li class="flex items-baseline justify-between gap-3">
            <span class={row.deferred ? 'text-fg-hint truncate' : 'truncate'}>{row.name}</span>
            <span class={row.deferred ? 'text-fg-hint tabular-nums' : 'tabular-nums'}>
              {formatTokens(row.tokens)}
            </span>
          </li>
        {/each}
      </ul>
      <!-- Deferred tool definitions are reported but NOT counted in
           totalTokens, so the rows deliberately don't sum to the total.
           Say so rather than letting the arithmetic look broken. -->
      {#if state.hasDeferred}
        <p class="mt-1 text-[0.625rem] text-fg-hint">Dimmed rows are not loaded into the prompt.</p>
      {/if}
    {/if}
  {/if}
</div>
