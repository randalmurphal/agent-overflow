<script module lang="ts">
  import { PANE_FIND_BAR_ATTR } from '../../utils/paneFindSeed';

  /** Types `text` at the caret of the find input that holds focus, as the
   * keyboard would. False when no find input does. */
  export function insertIntoFocusedFindInput(text: string): boolean {
    const input = document.activeElement;
    if (!(input instanceof HTMLInputElement) || !input.closest(`[${PANE_FIND_BAR_ATTR}]`)) return false;
    const end = input.value.length;
    input.setRangeText(text, input.selectionStart ?? end, input.selectionEnd ?? end, 'end');
    input.dispatchEvent(new Event('input', { bubbles: true }));
    return true;
  }
</script>

<script lang="ts">
  // The find bar every pane kind shares. Mounted directly under the pane's
  // header, full pane width, in the flow, so it pushes the content down
  // rather than covering it. It renders only while its pane's find is open
  // (stores/paneFind.svelte.ts) and owns the keys and focus of the session:
  // Enter / Shift+Enter step, and a close returns focus to the element that
  // had it when the bar opened (else `fallbackFocus`). A provider with no
  // `next` / `previous` is a filter: the bar shows how many results match,
  // has no step buttons, and Enter moves focus to the first result. Escape is not the
  // bar's: it bubbles to `find.close`, which decides which pane's find may
  // be dismissed (the focused pane, or the one on screen under compact).
  // While a count is pending the status says so in words for a screen
  // reader (the spinner is decorative); the words depend only on whether a
  // count shows, so a restart of the same count announces nothing new.
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
    focusFirstPaneFindResult,
    getPaneFind,
    getPaneFindProvider,
    isSteppingFind,
    setPaneFindBarFocused,
    setPaneFindQuery,
    stepPaneFind,
    takePaneFindReturnFocus,
  } from '../../stores/paneFind.svelte';
  import { isImeComposingEvent } from '../../utils/imeComposition';
  import { reportFrontendDiagnostic } from '../../utils/frontendErrorCapture';
  import { errString } from '../../utils/errors';

  interface Props {
    /** The find key the state is keyed by: a layout pane id or a region key. */
    paneId: string;
    /** The pane's title; labels the search landmark. */
    title: string;
    /**
     * Focus target when the element focused before the bar is gone or
     * cannot take focus. Must be focusable by script (a scroller takes
     * `tabindex="-1"`); one that is not is reported.
     */
    fallbackFocus: () => HTMLElement | null;
  }

  let { paneId, title, fallbackFocus }: Props = $props();

  let find = $derived(getPaneFind(paneId));
  let provider = $derived(getPaneFindProvider(paneId));
  let open = $derived(find?.open === true && provider !== null);
  let query = $derived(find?.query ?? '');
  let result = $derived(provider?.result ?? null);
  let count = $derived(result ? result.count : 0);
  let error = $derived(result?.error ?? null);
  // A progressive provider's count shows beside the spinner while it grows.
  let showCount = $derived(!result?.pending || (result.progressive === true && count !== null && count > 0));
  let stepping = $derived(provider !== null && isSteppingFind(provider));
  let countText = $derived.by(() => {
    if (!query.trim() || !result) return '';
    // A provider that cannot count reports a match without a number.
    if (count === null) return 'Found';
    if (!stepping) {
      if (count === 0) return result.pending ? '' : 'No matches';
      return `${count}${result.capped ? '+' : ''} ${count === 1 ? 'match' : 'matches'}`;
    }
    if (count === 0) return result.pending ? '' : 'No results';
    const at = result.current >= 0 ? `${result.current + 1}/` : '';
    return `${at}${count}${result.capped ? '+' : ''}`;
  });
  let pendingText = $derived.by(() => {
    if (!result?.pending || !query.trim() || (error && query.trim())) return '';
    return showCount && countText ? ', updating' : 'Searching';
  });
  let placeholder = $derived(stepping || !provider ? `Find in ${provider?.noun ?? 'pane'}` : `Filter ${provider.noun}`);

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
      if (target?.isConnected && takesFocus(target)) return;
      const fallback = fallbackFocus();
      if (fallback && !takesFocus(fallback)) {
        reportFrontendDiagnostic('pane find focus fallback not focusable', fallback.tagName);
      }
    });
  });

  function takesFocus(element: HTMLElement): boolean {
    element.focus({ preventScroll: true });
    return document.activeElement === element;
  }

  function handleInput(event: Event): void {
    setPaneFindQuery(paneId, (event.currentTarget as HTMLInputElement).value);
  }

  function handleInputKeydown(event: KeyboardEvent): void {
    if (event.key !== 'Enter' || isImeComposingEvent(event)) return;
    event.preventDefault();
    if (stepping) {
      stepPaneFind(paneId, event.shiftKey ? -1 : 1);
      return;
    }
    void focusFirstPaneFindResult(paneId).catch((err: unknown) => {
      reportFrontendDiagnostic('pane find focus first result failed', errString(err));
    });
  }

  // Reports whether keyboard focus is inside the bar, so a surface beneath
  // it (the terminal) knows its keys are the bar's.
  // Untracked: focus leaves while Svelte removes the bar, inside its block.
  function trackFocus(node: HTMLElement): () => void {
    const key = paneId;
    const report = (focused: boolean) => untrack(() => setPaneFindBarFocused(key, focused));
    // Focus moving inside the bar leaves and re-enters it in one turn.
    const onIn = () => report(true);
    const onOut = () => report(false);
    node.addEventListener('focusin', onIn);
    node.addEventListener('focusout', onOut);
    return () => {
      node.removeEventListener('focusin', onIn);
      node.removeEventListener('focusout', onOut);
      report(false);
    };
  }

  // An Escape that ends an IME composition belongs to the composition.
  function handleKeydown(event: KeyboardEvent): void {
    if (event.key === 'Escape' && isImeComposingEvent(event)) event.stopPropagation();
  }
</script>

{#if open}
  <!-- svelte-ignore a11y_no_noninteractive_element_interactions -->
  <div
    role="search"
    aria-label={stepping ? `Find in ${title}` : `Filter ${title}`}
    {...{ [PANE_FIND_BAR_ATTR]: '' }}
    data-testid="pane-find-bar"
    data-find-mode={stepping ? 'step' : 'filter'}
    class="flex h-8 compact:h-10 shrink-0 items-center gap-0.5 border-b border-border-subtle bg-surface-1 pl-3 pr-1.5"
    onkeydown={handleKeydown}
    {@attach trackFocus}
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
      {#if error && query.trim()}
        <span class="max-w-40 truncate text-error" title={error} role="alert" data-testid="pane-find-error">Find failed: {error}</span>
      {:else}
        {#if result?.pending}
          <span data-testid="pane-find-spinner" class="flex items-center {showCount && countText ? 'pr-1' : ''}"><SteppedSpinner size={11} /></span>
        {/if}
        {#if showCount}{countText}{/if}{#if pendingText}<span class="sr-only" data-testid="pane-find-pending">{pendingText}</span>{/if}
      {/if}
    </span>
    {#if stepping}
      <IconButton size="xs" label="Previous match" testId="pane-find-previous" disabled={count === 0} onClick={() => stepPaneFind(paneId, -1)}>
        {#snippet children()}<Icon icon={ChevronUp} size={14} />{/snippet}
      </IconButton>
      <IconButton size="xs" label="Next match" testId="pane-find-next" disabled={count === 0} onClick={() => stepPaneFind(paneId, 1)}>
        {#snippet children()}<Icon icon={ChevronDown} size={14} />{/snippet}
      </IconButton>
    {/if}
    <IconButton size="xs" label="Close find" testId="pane-find-close" onClick={() => closePaneFind(paneId)}>
      {#snippet children()}<Icon icon={X} size={13} />{/snippet}
    </IconButton>
  </div>
{/if}
