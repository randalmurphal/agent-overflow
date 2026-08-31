<script lang="ts">
  // Semantic menu container. Owns keyboard navigation (arrows, j/k,
  // Home/End) and the roving-tabindex pattern so screen readers and
  // keyboard users land on the right item.
  //
  // The menu does NOT own a popover — callers pair it with a Popover when
  // the menu is a dropdown, or use it standalone for inline menus
  // (e.g. a settings pane). Separating the two keeps this primitive
  // reusable.
  //
  // No typeahead. Menus that need filtering use an explicit text input
  // (CommandPalette, UnifiedThreadPicker, MessageSearch). Single-letter
  // keystrokes route to vim-style nav (j/k) instead.

  import type { Snippet } from 'svelte';

  interface Props {
    ariaLabel: string;
    children: Snippet;
    onClose?: () => void;
    /**
     * Tailwind class controlling the menu's minimum width. Defaults to
     * 200px, which fits the descriptions / long paths most pickers in
     * the composer toolbar render. Pickers with only short labels
     * with only very short labels should pass a
     * tighter value so the popup doesn't look balloon-wide.
     */
    minWidthClass?: string;
  }

  let {
    ariaLabel,
    children,
    onClose,
    minWidthClass = 'min-w-[200px]',
  }: Props = $props();

  let containerEl: HTMLDivElement | undefined = $state(undefined);

  // Return only enabled menuitems. Disabled items are visible for
  // context but skipped during keyboard navigation.
  function getItems(): HTMLElement[] {
    if (!containerEl) return [];
    const all = Array.from(
      containerEl.querySelectorAll<HTMLElement>('[role="menuitem"]'),
    );
    return all.filter((el) => el.getAttribute('aria-disabled') !== 'true');
  }

  function setFocus(index: number, items: HTMLElement[]): void {
    if (items.length === 0) return;
    const clamped = ((index % items.length) + items.length) % items.length;
    // Roving tabindex: only the focused item is tab-stop 0; the rest get
    // -1 so Tab jumps out of the menu rather than through every item.
    for (const [i, el] of items.entries()) {
      el.tabIndex = i === clamped ? 0 : -1;
    }
    items[clamped].focus();
  }

  function currentIndex(items: HTMLElement[]): number {
    const active = document.activeElement as HTMLElement | null;
    if (!active) return -1;
    return items.indexOf(active);
  }

  function handleKeydown(e: KeyboardEvent): void {
    const items = getItems();
    if (items.length === 0) return;
    const idx = currentIndex(items);

    // j/k mirror ArrowDown/ArrowUp when no modifier is held. With a
    // modifier they fall through so global chords (e.g. mod+j sidebar
    // cursor) still reach the window-level handler.
    const isPlainJ = e.key === 'j' && !e.ctrlKey && !e.metaKey && !e.altKey && !e.shiftKey;
    const isPlainK = e.key === 'k' && !e.ctrlKey && !e.metaKey && !e.altKey && !e.shiftKey;

    switch (e.key) {
      case 'ArrowDown':
        e.preventDefault();
        setFocus(idx < 0 ? 0 : idx + 1, items);
        return;
      case 'ArrowUp':
        e.preventDefault();
        setFocus(idx < 0 ? items.length - 1 : idx - 1, items);
        return;
      case 'Home':
        e.preventDefault();
        setFocus(0, items);
        return;
      case 'End':
        e.preventDefault();
        setFocus(items.length - 1, items);
        return;
      case 'Escape':
        if (onClose) {
          e.preventDefault();
          onClose();
        }
        return;
    }

    if (isPlainJ) {
      e.preventDefault();
      setFocus(idx < 0 ? 0 : idx + 1, items);
      return;
    }
    if (isPlainK) {
      e.preventDefault();
      setFocus(idx < 0 ? items.length - 1 : idx - 1, items);
      return;
    }
  }

  // Focus the first enabled menuitem on mount so keyboard users start
  // inside the menu without an extra Tab.
  //
  // Two wrinkles beyond the obvious `focus(0)` call:
  //
  // 1. The children snippet might hydrate synchronously OR async. The
  //    cold-cache ProviderModelsSubmenu and DiscussionsSubmenu both
  //    mount with a loading placeholder first and swap to the real
  //    items after a binding round-trip. A one-shot queueMicrotask
  //    would see 0 items and bail — leaving no item at tabindex=0
  //    and breaking keyboard navigation. We watch `containerEl` with
  //    a MutationObserver so the first batch of real items picks up
  //    initial focus whenever it lands.
  //
  // 2. Svelte reconciles `tabindex={-1}` on every MenuItem re-render,
  //    which wipes the roving-0 assignment `setFocus` applied. Each
  //    render flushes through the same observer callback, so the
  //    roving 0 is re-asserted whenever items mount/change.
  let focusInitialized = false;
  $effect(() => {
    if (!containerEl) return;
    const container = containerEl;

    const tryFocus = () => {
      if (focusInitialized) return;
      const items = getItems();
      if (items.length === 0) return;
      setFocus(0, items);
      focusInitialized = true;
    };

    queueMicrotask(tryFocus);

    // MutationObserver covers async-hydrated items. Once the first
    // focus lands, subsequent mutations don't re-steal focus because
    // `focusInitialized` is latched.
    const observer = new MutationObserver(tryFocus);
    observer.observe(container, { childList: true, subtree: true });

    return () => {
      observer.disconnect();
      focusInitialized = false;
    };
  });
</script>

<div
  bind:this={containerEl}
  role="menu"
  tabindex={-1}
  aria-orientation="vertical"
  aria-label={ariaLabel}
  onkeydown={handleKeydown}
  class="bg-surface-1 border border-border-subtle rounded-[var(--radius-control)] shadow-menu py-1 {minWidthClass} focus-visible:outline-none"
  data-menu
>
  {@render children()}
</div>
