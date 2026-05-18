<script lang="ts">
  import { onMount, onDestroy } from 'svelte';
  import { ensureMainPane, getFocusedPaneOrNull, getPane, openThreadFromNavigation } from './lib/stores/panes.svelte';
  import { setupEventListeners } from './lib/stores/events';
  import { getThreads, refreshThreads } from './lib/stores/threads.svelte';
  import { loadSettings, getSettings } from './lib/stores/settings.svelte';
  import { preloadProviderModelsForSettings } from './lib/stores/providerModels.svelte';
  import { applyTheme } from './lib/utils/theme';
  import { applyFonts } from './lib/utils/fonts';
  import Sidebar from './lib/components/sidebar/Sidebar.svelte';
  import PaneHost from './lib/components/panes/PaneHost.svelte';
  import Toast from './lib/components/shared/Toast.svelte';
  import TransportStatusBanner from './lib/components/shared/TransportStatusBanner.svelte';
  import SettingsView from './lib/components/settings/SettingsView.svelte';
  import DiscussionStartFlow from './lib/components/discussion/DiscussionStartFlow.svelte';
  import CommandPalette from './lib/components/palette/CommandPalette.svelte';
  import KeybindingsCheatSheet from './lib/components/palette/KeybindingsCheatSheet.svelte';
  import MessageSearch from './lib/components/palette/MessageSearch.svelte';
  import UnifiedThreadPicker from './lib/components/palette/UnifiedThreadPicker.svelte';
  import type { Thread } from './lib/types/models';
  import { getPaletteTargetPaneId, isPaletteOpen } from './lib/stores/palette.svelte';
  import { closeCheatSheet, isCheatSheetOpen } from './lib/stores/cheatSheet.svelte';
  import { closeMessageSearch, getMessageSearchTargetPaneId, isMessageSearchOpen } from './lib/stores/messageSearch.svelte';
  import { closeThreadPicker, getThreadPickerTargetPaneId, isThreadPickerOpen } from './lib/stores/threadPicker.svelte';
  import {
    dispatchKey,
    eventMatchesKeybindingCommand,
    loadKeybindings,
  } from './lib/stores/keybindings.svelte';
  import { clearCommandRegistry, type CommandContext } from './lib/stores/commandRegistry.svelte';
  import { registerBuiltinCommands, makeCommandContext } from './lib/stores/builtinCommands.svelte';
  import { installUiRenderTraceApi } from './lib/utils/uiRenderTrace';
  import { dispatchTextEditing } from './lib/utils/textEditingKeymap';
  import { installExternalLinkDelegate } from './lib/utils/externalLinks';
  import { getVisibleSidebarThreadIds } from './lib/stores/sidebarThreadOrder';
  import { setAppShellWidth } from './lib/stores/layoutMetrics.svelte';
  import DiagramInteractionHost from './lib/components/chat/DiagramInteractionHost.svelte';
  import { openDraftThreadForProject } from './lib/stores/threadCreation.svelte';
  import { addToast } from './lib/stores/toast.svelte';
  import { userFacingError } from './lib/utils/userFacingError';

  type SettingsSection = 'general' | 'providers' | 'editor' | 'network' | 'discussions' | 'keybindings' | 'observability' | 'archived';
  type SettingsContextTarget = {
    threadId?: string;
    provider: string;
    model: string;
    contextWindow?: number;
    autoCompactStandardPercent?: number;
    autoCompactExtendedPercent?: number;
  } | null;

  let showSettings = $state(false);
  let settingsSection = $state<SettingsSection>('general');
  let settingsContextTarget = $state<SettingsContextTarget>(null);
  let discussionStartFor = $state<Thread | null>(null);
  let searchFocuser = $state<(() => void) | null>(null);
  let openFromPR = $state<(() => void) | null>(null);
  let appContentEl: HTMLDivElement | undefined = $state(undefined);

  const pane = ensureMainPane();
  let sidebarPane = $derived(getFocusedPaneOrNull());
  const EDITABLE_REACHABLE_COMMANDS = new Set([
    'sidebar.focus-search',
    'palette.open',
    'mode.cycle',
    'rhs.close',
    'thread.interrupt',
    'thread.jump.1',
    'thread.jump.2',
    'thread.jump.3',
    'thread.jump.4',
    'thread.jump.5',
    'thread.jump.6',
    'thread.jump.7',
    'thread.jump.8',
    'thread.jump.9',
  ]);

  function handleStartDiscussion(thread: Thread): void {
    discussionStartFor = thread;
  }

  function closeDiscussionStart(): void {
    discussionStartFor = null;
  }

  function openSettings(section: SettingsSection = settingsSection, contextTarget: SettingsContextTarget = null): void {
    settingsSection = section;
    settingsContextTarget = contextTarget;
    showSettings = true;
  }

  function makeAppCommandContext(targetPane = getFocusedPaneOrNull() ?? pane) {
    return makeCommandContext(targetPane, {
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
    });
  }

  function makeCommandContextForPaneId(paneId: string): CommandContext | null {
    const targetPane = getPane(paneId);
    return targetPane ? makeAppCommandContext(targetPane) : null;
  }

  let paletteContext = $derived(
    makeAppCommandContext(getPane(getPaletteTargetPaneId() ?? '') ?? getFocusedPaneOrNull() ?? pane),
  );
  let messageSearchPane = $derived(
    getPane(getMessageSearchTargetPaneId() ?? '') ?? getFocusedPaneOrNull() ?? pane,
  );
  let threadPickerPane = $derived(
    getPane(getThreadPickerTargetPaneId() ?? '') ?? getFocusedPaneOrNull() ?? pane,
  );

  function handleGlobalKeydown(ev: KeyboardEvent): void {
    if (ev.defaultPrevented) return;
    // Word-op keymap (Alt/Ctrl + Backspace/Delete/Arrows) for any text
    // input. Cross-platform fill-in for the chord half each OS doesn't
    // bind natively. Runs ahead of the editable bail-out below.
    if (dispatchTextEditing(ev)) {
      ev.preventDefault();
      return;
    }
    // Let free-text inputs keep their typing behaviour. The palette overlay
    // mounts its own input handler that bypasses this branch naturally.
    const target = ev.target as HTMLElement | null;
    const tag = target?.tagName;
    const editable =
      tag === 'INPUT' ||
      tag === 'TEXTAREA' ||
      tag === 'SELECT' ||
      target?.isContentEditable === true;
    if (editable && !eventMatchesKeybindingCommand(ev, paletteContext, EDITABLE_REACHABLE_COMMANDS)) return;

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
    const ids = getVisibleSidebarThreadIds();
    const targetId = ids[index - 1];
    if (!targetId) return;
    const thread = getThreads().find((t) => t.id === targetId);
    if (thread) void openThreadFromNavigation(thread, getFocusedPaneOrNull() ?? ensureMainPane());
  }

  function requestThreadStep(delta: number): void {
    const ids = getVisibleSidebarThreadIds();
    if (ids.length === 0) return;
    const targetPane = getFocusedPaneOrNull() ?? ensureMainPane();
    const currentId = targetPane.threadId;
    const currentIndex = currentId ? ids.indexOf(currentId) : -1;
    const nextIndex = currentIndex === -1
      ? (delta > 0 ? 0 : ids.length - 1)
      : (currentIndex + delta + ids.length) % ids.length;
    const thread = getThreads().find((t) => t.id === ids[nextIndex]);
    if (thread) void openThreadFromNavigation(thread, targetPane);
  }

  function requestNewThread(openInNewPane: boolean): void {
    const targetPane = getFocusedPaneOrNull();
    const projectId = targetPane?.thread?.projectId;
    if (!projectId) {
      addToast('warning', 'Open a project thread before creating a new thread from the keyboard.');
      return;
    }
    void openDraftThreadForProject({
      projectId,
      mode: targetPane.activeTab,
      targetPane,
      openInNewPane,
    }).catch((err) => {
      console.error('Failed to create draft thread:', err);
      addToast('error', userFacingError(err));
    });
  }

  async function loadSettingsAndWarmModelCatalogs(): Promise<void> {
    const loaded = await loadSettings();
    if (!loaded) return;
    void preloadProviderModelsForSettings(getSettings());
  }

  $effect(() => {
    applyTheme(getSettings().theme);
  });

  $effect(() => {
    const s = getSettings();
    applyFonts(s.sansFont, s.monoFont);
  });

  $effect(() => {
    const el = appContentEl;
    if (!el) return;
    const obs = new ResizeObserver((entries) => {
      const entry = entries[0];
      if (entry) setAppShellWidth(entry.contentRect.width);
    });
    obs.observe(el);
    setAppShellWidth(el.getBoundingClientRect().width);
    return () => obs.disconnect();
  });

  onMount(() => {
    const cleanupEvents = setupEventListeners();
    refreshThreads();
    void loadSettingsAndWarmModelCatalogs();
    installUiRenderTraceApi();
    const cleanupExternalLinks = installExternalLinkDelegate();

    // Register the built-in commands. The hooks close over stable references
    // so commands see the live pane state each time they run.
    clearCommandRegistry();
    registerBuiltinCommands({
      openSettings: () => openSettings('general'),
      openThreadForm: () => requestNewThread(false),
      openThreadInNewPane: () => requestNewThread(true),
      openThreadFromPR: () => {
        // Sidebar registers a callback that flips its local `showFromPR`
        // state. The indirection keeps dialog ownership with the sidebar,
        // same pattern as focusThreadSearch above.
        openFromPR?.();
      },
      openShipChanges: (paneId) => {
        // The Ship Changes drawer lives inside GitActionsControl (deep in
        // the chat tree). A CustomEvent keeps App.svelte from owning a
        // reference to a component it never renders.
        window.dispatchEvent(new CustomEvent('agent-overflow:open-ship-changes', {
          detail: { paneId },
        }));
      },
      requestRename: requestRenameForThread,
      requestDiscussion: handleStartDiscussion,
      focusThreadSearch: () => searchFocuser?.(),
      requestThreadJump,
      requestThreadStep,
    });

    void loadKeybindings();
    const handleOpenSettings = (event: Event) => {
      const detail = (event as CustomEvent).detail as {
        section?: SettingsSection;
        contextTarget?: NonNullable<SettingsContextTarget>;
      } | undefined;
      openSettings(detail?.section ?? 'general', detail?.contextTarget ?? null);
    };
    window.addEventListener('agent-overflow:open-settings', handleOpenSettings);
    window.addEventListener('keydown', handleGlobalKeydown);

    return () => {
      cleanupEvents();
      cleanupExternalLinks();
      window.removeEventListener('agent-overflow:open-settings', handleOpenSettings);
    };
  });

  onDestroy(() => {
    if (typeof window !== 'undefined') {
      window.removeEventListener('keydown', handleGlobalKeydown);
    }
    clearCommandRegistry();
  });
</script>

{#snippet settingsSurface()}
  <SettingsView
    initialSection={settingsSection}
    contextTarget={settingsContextTarget}
    onClose={() => showSettings = false}
  />
{/snippet}

<main class="app-shell relative h-screen w-screen overflow-hidden text-text-primary flex flex-col">
  <TransportStatusBanner />
  <div bind:this={appContentEl} class="relative flex flex-1 min-h-0 w-full">
    <Sidebar
      pane={sidebarPane}
      onOpenSettings={() => openSettings('general')}
      registerFocusSearch={(focus) => (searchFocuser = focus)}
      registerOpenFromPR={(cb) => (openFromPR = cb)}
    />
    <PaneHost globalSurface={showSettings ? settingsSurface : undefined} />
  </div>
</main>
<DiscussionStartFlow
  open={discussionStartFor !== null}
  thread={discussionStartFor}
  {pane}
  onClose={closeDiscussionStart}
/>
<CommandPalette context={paletteContext} contextForPane={makeCommandContextForPaneId} />
<KeybindingsCheatSheet open={isCheatSheetOpen()} onClose={closeCheatSheet} />
<MessageSearch open={isMessageSearchOpen()} pane={messageSearchPane} onClose={closeMessageSearch} />
<UnifiedThreadPicker open={isThreadPickerOpen()} pane={threadPickerPane} onClose={closeThreadPicker} />
<Toast />
<DiagramInteractionHost />
