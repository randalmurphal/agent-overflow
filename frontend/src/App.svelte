<script lang="ts">
  import { onMount, onDestroy } from 'svelte';
  import { getMainPane } from './lib/stores/panes.svelte';
  import { setupEventListeners } from './lib/stores/events';
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
  import UnifiedThreadPicker from './lib/components/palette/UnifiedThreadPicker.svelte';
  import type { Thread } from './lib/types/models';
  import { isPaletteOpen } from './lib/stores/palette.svelte';
  import { closeCheatSheet, isCheatSheetOpen } from './lib/stores/cheatSheet.svelte';
  import { closeMessageSearch, isMessageSearchOpen } from './lib/stores/messageSearch.svelte';
  import { closeThreadPicker, isThreadPickerOpen } from './lib/stores/threadPicker.svelte';
  import {
    dispatchKey,
    loadKeybindings,
  } from './lib/stores/keybindings.svelte';
  import { clearCommandRegistry } from './lib/stores/commandRegistry.svelte';
  import { registerBuiltinCommands, makeCommandContext } from './lib/stores/builtinCommands.svelte';
  import { filterThreads } from './lib/stores/threadFilter.svelte';
  import { registerCodeCopyListener } from './lib/utils/codeCopy';
  import { registerMermaidRenderer } from './lib/utils/mermaidRenderer';
  import { registerMathRenderer } from './lib/utils/mathRenderer';
  import { installUiRenderTraceApi } from './lib/utils/uiRenderTrace';
  import DiagramInteractionHost from './lib/components/chat/DiagramInteractionHost.svelte';

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
      threadPickerOpen: isThreadPickerOpen(),
      anyModalOpen:
        discussionStartFor !== null ||
        showSettings ||
        isCheatSheetOpen() ||
        isMessageSearchOpen() ||
        isThreadPickerOpen(),
    }),
  );

  function handleGlobalKeydown(ev: KeyboardEvent): void {
    if (ev.defaultPrevented) return;
    // Let free-text inputs keep their typing behaviour. The palette overlay
    // mounts its own input handler that bypasses this branch naturally.
    const target = ev.target as HTMLElement | null;
    const tag = target?.tagName;
    const editable =
      tag === 'INPUT' ||
      tag === 'TEXTAREA' ||
      tag === 'SELECT' ||
      target?.isContentEditable === true;
    // Allow Cmd/Ctrl+K even from editable elements so the sidebar search
    // and palette chords (⌘K / ⌘⇧K) are always reachable. Shift+Tab is
    // also allowed through so `mode.cycle` works while the composer
    // textarea has focus — the textarea itself preventDefaults the key
    // to suppress the browser's outdent behaviour.
    const isSidebarOrPaletteChord =
      (ev.metaKey || ev.ctrlKey) && ev.key.toLowerCase() === 'k' && !ev.altKey;
    const isShiftTab = ev.key === 'Tab' && ev.shiftKey && !ev.metaKey && !ev.ctrlKey && !ev.altKey;
    const isPlainEscape =
      ev.key === 'Escape' && !ev.metaKey && !ev.ctrlKey && !ev.altKey && !ev.shiftKey;
    if (editable && !isSidebarOrPaletteChord && !isShiftTab && !isPlainEscape) return;

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
    refreshThreads();
    loadSettings();
    registerCodeCopyListener();
    registerMermaidRenderer();
    registerMathRenderer();
    installUiRenderTraceApi();

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
  <div class="relative flex h-full w-full">
    <Sidebar
      {pane}
      onOpenSettings={() => showSettings = true}
      registerFocusSearch={(focus) => (searchFocuser = focus)}
      registerOpenFromPR={(cb) => (openFromPR = cb)}
    />
    <div class="flex-1 flex flex-col min-w-0">
      {#if showSettings}
        <SettingsView onClose={() => showSettings = false} />
      {:else}
        {#key pane.threadId}
          <ChatView {pane} />
        {/key}
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
<UnifiedThreadPicker open={isThreadPickerOpen()} {pane} onClose={closeThreadPicker} />
<Toast />
<DiagramInteractionHost />
