<script lang="ts">
  import { onMount, onDestroy } from 'svelte';
  import { getMainPane } from './lib/stores/panes.svelte';
  import { setupEventListeners } from './lib/stores/events';
  import { setupProviderStatusListener } from './lib/stores/providerStatus.svelte';
  import { getThreads, refreshThreads } from './lib/stores/threads.svelte';
  import { loadSettings, getSettings } from './lib/stores/settings.svelte';
  import { applyTheme } from './lib/utils/theme';
  import Sidebar from './lib/components/sidebar/Sidebar.svelte';
  import ChatView from './lib/components/chat/ChatView.svelte';
  import Toast from './lib/components/shared/Toast.svelte';
  import SettingsView from './lib/components/settings/SettingsView.svelte';
  import DiscussionStartFlow from './lib/components/discussion/DiscussionStartFlow.svelte';
  import CommandPalette from './lib/components/palette/CommandPalette.svelte';
  import KeybindingsCheatSheet from './lib/components/palette/KeybindingsCheatSheet.svelte';
  import MessageSearch from './lib/components/palette/MessageSearch.svelte';
  import type { Thread } from './lib/types/models';
  import { isPaletteOpen } from './lib/stores/palette.svelte';
  import { closeCheatSheet, isCheatSheetOpen } from './lib/stores/cheatSheet.svelte';
  import { closeMessageSearch, isMessageSearchOpen } from './lib/stores/messageSearch.svelte';
  import {
    dispatchKey,
    loadKeybindings,
  } from './lib/stores/keybindings.svelte';
  import { clearCommandRegistry } from './lib/stores/commandRegistry.svelte';
  import { registerBuiltinCommands, makeCommandContext } from './lib/stores/builtinCommands.svelte';
  import { filterThreads } from './lib/stores/threadFilter.svelte';

  let showSettings = $state(false);
  let discussionStartFor = $state<Thread | null>(null);
  let searchFocuser = $state<(() => void) | null>(null);
  let openFromPR = $state<(() => void) | null>(null);

  const pane = getMainPane();

  function handleStartDiscussion(thread: Thread): void {
    discussionStartFor = thread;
  }

  function closeDiscussionStart(): void {
    discussionStartFor = null;
  }

  let paletteContext = $derived(
    makeCommandContext(pane, {
      paletteOpen: isPaletteOpen(),
      cheatSheetOpen: isCheatSheetOpen(),
      messageSearchOpen: isMessageSearchOpen(),
      anyModalOpen:
        discussionStartFor !== null ||
        showSettings ||
        isCheatSheetOpen() ||
        isMessageSearchOpen(),
    }),
  );

  function handleGlobalKeydown(ev: KeyboardEvent): void {
    // Let free-text inputs keep their typing behaviour. The palette overlay
    // mounts its own input handler that bypasses this branch naturally.
    const target = ev.target as HTMLElement | null;
    const tag = target?.tagName;
    const editable =
      tag === 'INPUT' ||
      tag === 'TEXTAREA' ||
      tag === 'SELECT' ||
      target?.isContentEditable === true;
    // Allow Cmd/Ctrl+K even from editable elements so the palette is always
    // reachable. Everything else respects editable context.
    const isPaletteChord = (ev.metaKey || ev.ctrlKey) && ev.key.toLowerCase() === 'k' && !ev.shiftKey && !ev.altKey;
    if (editable && !isPaletteChord) return;

    const handled = dispatchKey(ev, paletteContext);
    if (handled) ev.preventDefault();
  }

  function requestRenameForThread(thread: Thread): void {
    // Sidebar currently owns inline rename via ThreadRow. We surface the
    // rename request as a CustomEvent so the (future) rename modal or the
    // sidebar row can pick it up; until then the event is documentation.
    window.dispatchEvent(new CustomEvent('agent-overflow:rename-thread', { detail: thread }));
  }

  function requestThreadJump(index: number): void {
    const threads = filterThreads(getThreads());
    const target = threads[index - 1];
    if (!target) return;
    void pane.switchThread(target);
  }

  function requestThreadStep(delta: number): void {
    const threads = filterThreads(getThreads());
    if (threads.length === 0) return;
    const currentId = pane.threadId;
    const currentIndex = currentId ? threads.findIndex((t) => t.id === currentId) : -1;
    const nextIndex = currentIndex === -1
      ? (delta > 0 ? 0 : threads.length - 1)
      : (currentIndex + delta + threads.length) % threads.length;
    void pane.switchThread(threads[nextIndex]);
  }

  $effect(() => {
    applyTheme(getSettings().theme);
  });

  onMount(() => {
    const cleanupEvents = setupEventListeners();
    const cleanupProviderStatus = setupProviderStatusListener();
    refreshThreads();
    loadSettings();

    // Register the built-in commands. The hooks close over stable references
    // so commands see the live pane state each time they run.
    clearCommandRegistry();
    registerBuiltinCommands({
      pane,
      openSettings: () => (showSettings = true),
      openThreadForm: () => {
        // The sidebar's "+ New Thread" button owns the form today. Firing a
        // CustomEvent keeps the contract loose; the sidebar can listen for
        // this event in a future wave without an API rename here.
        window.dispatchEvent(new CustomEvent('agent-overflow:open-thread-form'));
      },
      openThreadFromPR: () => {
        // Sidebar registers a callback that flips its local `showFromPR`
        // state. The indirection keeps dialog ownership with the sidebar,
        // same pattern as focusThreadSearch above.
        openFromPR?.();
      },
      openShipChanges: () => {
        // The Ship Changes drawer lives inside GitActionsControl (deep in
        // the chat tree). A CustomEvent keeps App.svelte from owning a
        // reference to a component it never renders.
        window.dispatchEvent(new CustomEvent('agent-overflow:open-ship-changes'));
      },
      requestRename: requestRenameForThread,
      requestDiscussion: handleStartDiscussion,
      focusThreadSearch: () => searchFocuser?.(),
      requestThreadJump,
      requestThreadStep,
    });

    void loadKeybindings();
    window.addEventListener('keydown', handleGlobalKeydown);

    return () => {
      cleanupEvents();
      cleanupProviderStatus();
    };
  });

  onDestroy(() => {
    if (typeof window !== 'undefined') {
      window.removeEventListener('keydown', handleGlobalKeydown);
    }
    clearCommandRegistry();
  });
</script>

<main class="app-shell relative h-screen w-screen overflow-hidden text-text-primary">
  <div class="pointer-events-none absolute inset-0 opacity-70">
    <div class="absolute left-[-12rem] top-[-10rem] h-[28rem] w-[28rem] rounded-full bg-accent/12 blur-3xl"></div>
    <div class="absolute bottom-[-14rem] right-[-10rem] h-[24rem] w-[24rem] rounded-full bg-provider-codex/10 blur-3xl"></div>
  </div>
  <div class="relative flex h-full w-full">
    <Sidebar
      {pane}
      onOpenSettings={() => showSettings = true}
      onStartDiscussion={handleStartDiscussion}
      registerFocusSearch={(focus) => (searchFocuser = focus)}
      registerOpenFromPR={(cb) => (openFromPR = cb)}
    />
    <div class="flex-1 flex flex-col min-w-0">
      {#if showSettings}
        <SettingsView onClose={() => showSettings = false} />
      {:else}
        <ChatView {pane} />
      {/if}
    </div>
  </div>
</main>
<DiscussionStartFlow
  open={discussionStartFor !== null}
  thread={discussionStartFor}
  {pane}
  onClose={closeDiscussionStart}
/>
<CommandPalette context={paletteContext} />
<KeybindingsCheatSheet open={isCheatSheetOpen()} onClose={closeCheatSheet} />
<MessageSearch open={isMessageSearchOpen()} {pane} onClose={closeMessageSearch} />
<Toast />
