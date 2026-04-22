<script lang="ts">
  // Semantic menu container. Owns keyboard navigation (arrows, Home/End,
  // typeahead) and the roving-tabindex pattern so screen readers and
  // keyboard users land on the right item.
  //
  // The menu does NOT own a popover — callers pair it with a Popover when
  // the menu is a dropdown, or use it standalone for inline menus
  // (e.g. a settings pane). Separating the two keeps this primitive
  // reusable.

  import type { Snippet } from 'svelte';

  interface Props {
    ariaLabel: string;
    children: Snippet;
    onClose?: () => void;
  }

  let { ariaLabel, children, onClose }: Props = $props();

  let containerEl: HTMLDivElement | undefined = $state(undefined);

  // Typeahead buffer: characters accumulate within a 750ms window so
  // users can type "con" to jump to "Connect". Matching is prefix-only
  // and case-insensitive.
  let typeaheadBuffer = $state('');
  let typeaheadTimer: ReturnType<typeof setTimeout> | null = null;

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

  function clearTypeahead(): void {
    typeaheadBuffer = '';
    if (typeaheadTimer) {
      clearTimeout(typeaheadTimer);
      typeaheadTimer = null;
    }
  }

  function handleTypeahead(char: string): void {
    typeaheadBuffer += char.toLowerCase();
    if (typeaheadTimer) clearTimeout(typeaheadTimer);
    typeaheadTimer = setTimeout(() => {
      typeaheadBuffer = '';
      typeaheadTimer = null;
    }, 750);

    const items = getItems();
    if (items.length === 0) return;
    // Search starting from the item after the currently-focused one so
    // repeated presses of the same letter rotate through matches.
    const idx = currentIndex(items);
    const start = idx >= 0 ? (idx + (typeaheadBuffer.length === 1 ? 1 : 0)) % items.length : 0;
    for (let step = 0; step < items.length; step++) {
      const probe = (start + step) % items.length;
      const text = (items[probe].textContent ?? '').trim().toLowerCase();
      if (text.startsWith(typeaheadBuffer)) {
        setFocus(probe, items);
        return;
      }
    }
  }

  function handleKeydown(e: KeyboardEvent): void {
    const items = getItems();
    if (items.length === 0) return;
    const idx = currentIndex(items);

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

    // Printable single-character keys feed typeahead. Filter out
    // modifiers so Cmd+K or Ctrl+Shift+X doesn't accidentally drive
    // the buffer.
    if (e.key.length === 1 && !e.ctrlKey && !e.metaKey && !e.altKey) {
      e.preventDefault();
      handleTypeahead(e.key);
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
    // focus lands, subsequent mutations (navigation, typeahead, etc.)
    // don't re-steal focus because `focusInitialized` is latched.
    const observer = new MutationObserver(tryFocus);
    observer.observe(container, { childList: true, subtree: true });

    return () => {
      observer.disconnect();
      clearTypeahead();
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
  class="bg-surface-1 border border-border-subtle rounded-[var(--radius-control)] shadow-menu py-1 min-w-[200px] focus-visible:outline-none"
  data-menu
>
  {@render children()}
</div>
