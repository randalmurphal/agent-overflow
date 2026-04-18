<script lang="ts">
  // Generic "too much chrome" trigger + dropdown. Collapses arbitrary
  // toolbar children behind a single button so a narrow header stays
  // tidy. The component owns only its open/close state — the caller
  // decides what goes inside via the `children` snippet.
  //
  // The snippet receives `onClose` so a menu item can dismiss the
  // popover after it finishes its work (e.g. after a picker inside
  // commits a selection). Click / Escape / backdrop close are handled
  // here.

  import { onDestroy, type Snippet } from 'svelte';
  import { fade, fly } from 'svelte/transition';

  let {
    children,
    label = 'More',
  }: {
    children: Snippet<[{ onClose: () => void }]>;
    label?: string;
  } = $props();

  let open = $state(false);
  let triggerEl: HTMLButtonElement | undefined = $state(undefined);
  let menuEl: HTMLDivElement | undefined = $state(undefined);

  function close() {
    open = false;
    triggerEl?.focus();
  }

  // Keep the focus trap scoped to visibly-focusable elements inside the
  // menu. Callers nest their own popovers (ModelPicker, BranchToolbar)
  // which render their dropdown as a sibling — those dropdowns sit
  // outside `menuEl` and the backdrop handler below naturally allows
  // interaction with them.
  function focusableChildren(): HTMLElement[] {
    if (!menuEl) return [];
    const selector =
      'button:not([disabled]), a[href], input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])';
    return Array.from(menuEl.querySelectorAll<HTMLElement>(selector));
  }

  // Focus the first interactive child when the menu opens so keyboard
  // users don't have to tab past the backdrop.
  $effect(() => {
    if (open && menuEl) {
      const first = focusableChildren()[0];
      first?.focus();
    }
  });

  function handleMenuKeydown(e: KeyboardEvent) {
    if (e.key === 'Escape') {
      e.preventDefault();
      close();
      return;
    }
    if (e.key !== 'Tab') return;
    const items = focusableChildren();
    if (items.length === 0) return;
    const first = items[0];
    const last = items[items.length - 1];
    const active = document.activeElement as HTMLElement | null;
    if (e.shiftKey && active === first) {
      e.preventDefault();
      last.focus();
    } else if (!e.shiftKey && active === last) {
      e.preventDefault();
      first.focus();
    }
  }

  function handleBackdropClick(e: MouseEvent) {
    if (e.target === e.currentTarget) {
      close();
    }
  }

  function handleBackdropKeydown(e: KeyboardEvent) {
    if (e.key === 'Escape') {
      close();
    }
  }

  // If the component unmounts while the dropdown is open (e.g. header
  // resizes back to wide and we stop rendering the trigger), the fixed
  // backdrop would otherwise linger. Svelte handles cleanup of the
  // rendered nodes; this is a defensive reset for any stray listener
  // state a future addition might attach.
  onDestroy(() => {
    open = false;
  });
</script>

<div class="relative inline-flex">
  <button
    bind:this={triggerEl}
    type="button"
    onclick={() => { open = !open; }}
    aria-haspopup="menu"
    aria-expanded={open}
    aria-label={label}
    data-testid="compact-header-menu-trigger"
    class="rounded border border-border px-2 py-0.5 text-xs text-text-secondary hover:bg-surface-2/60 cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
  >
    {label}
  </button>

  {#if open}
    <!-- svelte-ignore a11y_no_static_element_interactions -->
    <div
      transition:fade={{ duration: 100 }}
      class="fixed inset-0 z-40"
      data-testid="compact-header-menu-backdrop"
      onclick={handleBackdropClick}
      onkeydown={handleBackdropKeydown}
    ></div>
    <!-- svelte-ignore a11y_interactive_supports_focus -->
    <div
      bind:this={menuEl}
      role="menu"
      aria-label={label}
      data-testid="compact-header-menu"
      onkeydown={handleMenuKeydown}
      transition:fly={{ y: -4, duration: 120 }}
      class="absolute top-full right-0 mt-1 z-50 bg-surface-1 border border-border rounded-lg shadow-xl w-[260px] p-2 flex flex-col gap-1.5 items-stretch"
    >
      {@render children({ onClose: close })}
    </div>
  {/if}
</div>
