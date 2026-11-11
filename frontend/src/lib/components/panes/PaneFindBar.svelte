<script lang="ts">
  // The find bar every pane kind shares. Mounted directly under the pane's
  // header, full pane width, in the flow, so it pushes the content down
  // rather than covering it. It renders only while its pane's find is open
  // (stores/paneFind.svelte.ts) and owns the keys and focus of the session:
  // Enter / Shift+Enter step, Escape closes, a close returns focus to the
  // element that had it when the bar opened (else `fallbackFocus`).
  import { tick, untrack } from 'svelte';
  import Search from '@lucide/svelte/icons/search';
  import ChevronUp from '@lucide/svelte/icons/chevron-up';
  import ChevronDown from '@lucide/svelte/icons/chevron-down';
  import X from '@lucide/svelte/icons/x';
  import Icon from '../primitives/Icon.svelte';
  import IconButton from '../primitives/IconButton.svelte';
  import SteppedSpinner from '../primitives/SteppedSpinner.svelte';
  import {
    closePaneFind,
    getPaneFind,
    getPaneFindProvider,
    setPaneFindQuery,
    stepPaneFind,
    takePaneFindReturnFocus,
  } from '../../stores/paneFind.svelte';
  import { isImeComposingEvent } from '../../utils/imeComposition';
  import { PANE_FIND_BAR_ATTR } from '../../utils/paneFindSeed';

  interface Props {
    /** The layout pane id the find state is keyed by. */
    paneId: string;
    /** The pane's title; labels the search landmark. */
    title: string;
    /** Focus target when the element focused before the bar is gone. */
    fallbackFocus: () => HTMLElement | null;
  }

  let { paneId, title, fallbackFocus }: Props = $props();

  let find = $derived(getPaneFind(paneId));
  let provider = $derived(getPaneFindProvider(paneId));
  let open = $derived(find?.open === true && provider !== null);
  let query = $derived(find?.query ?? '');
  let result = $derived(provider?.result ?? null);
  let count = $derived(result?.count ?? 0);
  let countText = $derived.by(() => {
    if (!query.trim() || !result) return '';
    if (count === 0) return 'No results';
    return `${result.current + 1}/${count}${result.capped ? '+' : ''}`;
  });
  let placeholder = $derived(`Find in ${provider?.noun ?? 'pane'}`);

  let inputEl: HTMLInputElement | undefined = $state(undefined);

  // Every open asks for the input: focus it and select the query. A request
  // that predates this mount (a body remount while open) is not a new ask.
  let seenFocusRequest = untrack(() => getPaneFind(paneId)?.focusRequest ?? 0);
  $effect(() => {
    const request = find?.focusRequest ?? 0;
    const input = inputEl;
    if (!open || !input || request === seenFocusRequest) return;
    seenFocusRequest = request;
    input.focus({ preventScroll: true });
    input.select();
  });

  // A close returns focus when it left focus nowhere (the bar's own input or
  // button was removed with it), never when the reader already moved on.
  let wasOpen = untrack(() => open);
  $effect(() => {
    const nowOpen = open;
    if (nowOpen || !wasOpen) {
      wasOpen = nowOpen;
      return;
    }
    wasOpen = false;
    const target = untrack(() => takePaneFindReturnFocus(paneId));
    void tick().then(() => {
      const active = document.activeElement;
      if (active && active !== document.body && active.isConnected) return;
      const next = target?.isConnected ? target : fallbackFocus();
      next?.focus({ preventScroll: true });
    });
  });

  function handleInput(event: Event): void {
    setPaneFindQuery(paneId, (event.currentTarget as HTMLInputElement).value);
  }

  function handleInputKeydown(event: KeyboardEvent): void {
    if (event.key !== 'Enter' || isImeComposingEvent(event)) return;
    event.preventDefault();
    stepPaneFind(paneId, event.shiftKey ? -1 : 1);
  }

  function handleKeydown(event: KeyboardEvent): void {
    if (event.key !== 'Escape' || isImeComposingEvent(event)) return;
    event.preventDefault();
    event.stopPropagation();
    closePaneFind(paneId);
  }
</script>

{#if open}
  <!-- svelte-ignore a11y_no_noninteractive_element_interactions -->
  <div
    role="search"
    aria-label={`Find in ${title}`}
    {...{ [PANE_FIND_BAR_ATTR]: '' }}
    data-testid="pane-find-bar"
    class="flex h-8 compact:h-10 shrink-0 items-center gap-0.5 border-b border-border-subtle bg-surface-1 pl-3 pr-1.5"
    onkeydown={handleKeydown}
  >
    <Icon icon={Search} size={13} class="shrink-0 text-fg-subtle" />
    <input
      bind:this={inputEl}
      type="text"
      value={query}
      {placeholder}
      aria-label={placeholder}
      spellcheck="false"
      autocomplete="off"
      data-testid="pane-find-input"
      class="h-full min-w-0 flex-1 border-0 bg-transparent px-2 text-[0.8125rem] compact:text-base text-fg placeholder:text-fg-hint focus:outline-none"
      oninput={handleInput}
      onkeydown={handleInputKeydown}
    />
    <span
      aria-live="polite"
      class="flex shrink-0 items-center px-1 text-[0.75rem] tabular-nums text-fg-subtle"
      data-testid="pane-find-count"
    >
      {#if result?.pending}
        <span data-testid="pane-find-spinner" class="flex items-center"><SteppedSpinner size={11} /></span>
      {:else}
        {countText}
      {/if}
    </span>
    <IconButton size="xs" label="Previous match" testId="pane-find-previous" disabled={count === 0} onClick={() => stepPaneFind(paneId, -1)}>
      {#snippet children()}<Icon icon={ChevronUp} size={14} />{/snippet}
    </IconButton>
    <IconButton size="xs" label="Next match" testId="pane-find-next" disabled={count === 0} onClick={() => stepPaneFind(paneId, 1)}>
      {#snippet children()}<Icon icon={ChevronDown} size={14} />{/snippet}
    </IconButton>
    <IconButton size="xs" label="Close find" testId="pane-find-close" onClick={() => closePaneFind(paneId)}>
      {#snippet children()}<Icon icon={X} size={13} />{/snippet}
    </IconButton>
  </div>
{/if}
